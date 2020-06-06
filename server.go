// This app implements RFC8210.
// The Resource Public Key Infrastructure (RPKI) to Router Protocol.
// Version 1

package main

import (
	"fmt"
	"log"
	"math/rand"
	"net"
	"os"
	"path"
	"sync"
	"time"

	"gopkg.in/ini.v1"
)

type rir uint8

const (
	cacheurl = "https://rpki.cloudflare.com/rpki.json"

	// Each region will just be an enum.
	afrinic rir = 0
	apnic   rir = 1
	arin    rir = 2
	lacnic  rir = 3
	ripe    rir = 4

	// refreshROA is the amount of seconds to wait until a new json is pulled.
	refreshROA = 15 * time.Minute

	// Intervals are the default intervals in seconds if no specific value is configured
	refresh = uint32(3600) // 1 - 86400
	retry   = uint32(600)  // 1 - 7200
	expire  = uint32(7200) // 600 - 172800

	//maxMinMask is the largest min mask wanted
	maxMinMaskv4 = 24
	maxMinMaskv6 = 48
)

// jsonroa is a struct to push the cloudflare ROA data into.
type jsonroa struct {
	Prefix string  `json:"prefix"`
	Mask   float64 `json:"maxLength"`
	ASN    string  `json:"asn"`
	RIR    string  `json:"ta"`
}

// Converted ROA struct with all the details.
// Handy calculator - http://golang-sizeof.tips/ - current size 24
type roa struct {
	Prefix  string
	MinMask uint8
	MaxMask uint8
	ASN     uint32
	RIR     rir
	IsV4    bool
}

// rpkiResponse, metadata, and roas are all used to unmarshal the json file.
type rpkiResponse struct {
	metadata `json:"metadata"`
	roas
}

type metadata struct {
	Generated float64 `json:"generated"`
	Valid     float64 `json:"valid"`
}

type roas struct {
	Roas []jsonroa `json:"roas"`
}

// CacheServer is our RPKI cache server.
type CacheServer struct {
	listener net.Listener
	clients  []*client
	roas     []roa
	mutex    *sync.RWMutex
	serial   uint32
	session  uint16
	diff     serialDiff
	updates  checkErrorUpdate
}

// checkErrorUpdate will let us know timings of ROA updates.
type checkErrorUpdate struct {
	lastCheck  time.Time
	lastError  time.Time
	lastUpdate time.Time
}

// serialDiff will have a list of add and deletes of ROAs to get from
// oldSerial to newSerial.
type serialDiff struct {
	oldSerial uint32
	newSerial uint32
	delRoa    []roa
	addRoa    []roa
	// There may be no actual diffs between now and last
	diff bool
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "%s\n", err)
		os.Exit(1)
	}
}

// run will do the initial set up. Returns error to main.
func run() error {
	// load in config
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	path := fmt.Sprintf("%s/config.ini", path.Dir(exe))
	cf, err := ini.Load(path)
	if err != nil {
		log.Fatalf("failed to read config file: %v\n", err)
	}
	logf := cf.Section("rpkirtr").Key("log").String()
	port, err := cf.Section("rpkirtr").Key("port").Int64()
	if err != nil {
		return fmt.Errorf("Port set needs to be a number: %v", err)
	}

	// set up logging
	f, err := os.OpenFile(logf, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("failed to open logfile: %w", err)
	}
	defer f.Close()
	log.SetOutput(f)

	// random seed used for session ID
	rand.Seed(time.Now().UTC().UnixNano())

	// We need our initial set of ROAs.
	log.Printf("Downloading %s\n", cacheurl)
	roas, err := readROAs(cacheurl)
	init := time.Now() // Use this value to save time of first roa update.
	if err != nil {
		return fmt.Errorf("Unable to download ROAs, aborting: %w", err)
	}
	log.Println("Initial roa set downloaded")

	// Set up our server with it's initial data.
	rpki := CacheServer{
		mutex:   &sync.RWMutex{},
		session: uint16(rand.Intn(65535)),
		roas:    roas,
		updates: checkErrorUpdate{
			lastCheck: init,
		},
	}

	// keep ROAs updated.
	go rpki.updateROAs(cacheurl)

	go rpki.status()

	// I'm listening!
	rpki.listen(port)
	defer rpki.close()
	rpki.start()

	return nil

}

// Start listening
func (s *CacheServer) listen(port int64) {
	l, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		log.Fatalf("Unable to start server: %v", err)
	}
	s.listener = l
	log.Printf("Listening on port %d\n", port)

}

// Log current ROA status
func (s *CacheServer) status() {
	for {
		s.mutex.RLock()
		// Count how many ROAs we have.
		var v4, v6 int
		for _, r := range s.roas {
			if r.IsV4 {
				v4++
			} else {
				v6++
			}
		}

		log.Println("*** Status ***")
		log.Printf("I currently have %d clients connected\n", len(s.clients))
		for i, v := range s.clients {
			log.Printf("%d: %s\n", i+1, v.addr)
		}
		log.Printf("Current serial number is %d\n", s.serial)
		log.Printf("Last diff is %t\n", s.diff.diff)
		log.Printf("Current size of diff is %d\n", len(s.diff.addRoa)+len(s.diff.delRoa))
		log.Printf("There are %d ROAs\n", len(s.roas))
		log.Printf("There are %d IPv4 ROAs and %d IPv6 ROAs\n", v4, v6)
		if !s.updates.lastCheck.IsZero() {
			log.Printf("Last check was %v\n", s.updates.lastCheck.Format("2006-01-02 15:04:05"))
		}
		if !s.updates.lastError.IsZero() {
			log.Printf("Last error checking update was %v\n", s.updates.lastError.Format("2006-01-02 15:04:05"))
		}
		if !s.updates.lastUpdate.IsZero() {
			log.Printf("Last ROA change was %v\n", s.updates.lastUpdate.Format("2006-01-02 15:04:05"))
		}
		log.Println("*** eom ***")
		s.mutex.RUnlock()
		time.Sleep(refreshROA)
	}

}

// close off the listener if existing
func (s *CacheServer) close() {
	s.listener.Close()
}

// start will start the listener as well as accept client and handle each.
func (s *CacheServer) start() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			log.Printf("%v\n", err)
		} else {
			client := s.accept(conn)
			go s.handleClient(client)
		}
	}
}

// accept adds a new client to the current list of clients being served.
func (s *CacheServer) accept(conn net.Conn) *client {
	log.Printf("Connection from %v, total clients: %d\n",
		conn.RemoteAddr().String(), len(s.clients)+1)

	s.mutex.Lock()
	defer s.mutex.Unlock()

	ip, _, _ := net.SplitHostPort(conn.RemoteAddr().String())

	// Each client will have a pointer to a load of the server's data.
	client := &client{
		conn:   conn,
		addr:   ip,
		roas:   &s.roas,
		serial: &s.serial,
		mutex:  s.mutex,
		diff:   &s.diff,
	}

	s.clients = append(s.clients, client)

	return client
}

// remove removes a client from the current list of clients being served.
func (s *CacheServer) remove(c *client) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	log.Printf("Removing client %s\n", c.conn.RemoteAddr().String())

	// remove the connection from client array
	for i, check := range s.clients {
		if check == c {
			s.clients = append(s.clients[:i], s.clients[i+1:]...)
		}
	}
}

// updateROAs will update the server struct with the current list of ROAs
func (s *CacheServer) updateROAs(f string) {
	for {
		time.Sleep(refreshROA)
		s.mutex.Lock()
		s.updates.lastCheck = time.Now()
		roas, err := readROAs(f)
		if err != nil {
			log.Printf("Unable to update ROAs, so keeping existing ROAs for now: %v\n", err)
			s.updates.lastError = time.Now()
			s.mutex.Unlock()
			return
		}

		// Calculate diffs
		s.diff = makeDiff(roas, s.roas, s.serial)
		if s.diff.diff {
			s.updates.lastUpdate = time.Now()
		}

		// Increment serial and replace
		s.serial++
		s.roas = roas
		log.Printf("roas updated, serial is now %d\n", s.serial)

		s.mutex.Unlock()
		// Notify all clients that the serial number has been updated.
		for _, c := range s.clients {
			log.Printf("sending a notify to %s\n", c.addr)
			c.notify(s.serial, s.session)
		}
	}
}
