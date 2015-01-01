package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sorcix/irc"
)

var (
	servers = flag.String("servers",
		"localhost:8001",
		"(comma-separated) list of host:port network addresses of the server(s) to connect to")

	listen = flag.String("listen",
		"localhost:6667",
		"host:port to listen on for IRC connections")

	currentMaster string
	allServers    []string
)

// TODO: move this to a common package, perhaps fancyirc/types?
type fancyId int64
type fancyMessage struct {
	Id   fancyId
	Data string
}

// TODO(secure): persistent state:
// - the follow targets (channels)
// - the last known server(s) in the network. added to *servers
// - the last seen message id
// for hosted mode, this state is stored per-nickname, ideally encrypted with password

func sendFancyMessage(logPrefix string, master string, data []byte) error {
	res, err := http.Post(
		fmt.Sprintf("http://%s/put", master),
		"application/json",
		bytes.NewBuffer(data))

	if err != nil {
		// TODO(secure): try one of the other servers.
		return err
	}

	if res.StatusCode == http.StatusTemporaryRedirect {
		loc := res.Header.Get("Location")
		if loc == "" {
			return fmt.Errorf("Redirect has no Location header")
		}
		u, err := url.Parse(loc)
		if err != nil {
			return fmt.Errorf("Could not parse redirection %q: %v", loc, err)
		}

		return sendFancyMessage(logPrefix, u.Host, data)
	}

	if res.StatusCode != 200 {
		data, _ := ioutil.ReadAll(res.Body)
		return fmt.Errorf("sendFancyMessage() failed: %s", string(data))
	}

	log.Printf("%s ->fancy: %q\n", logPrefix, string(data))

	currentMaster = master
	return nil
}

func sendIRCMessage(logPrefix string, ircConn *irc.Conn, msg irc.Message) {
	if err := ircConn.Encode(&msg); err != nil {
		log.Printf("%s Error sending IRC message %q: %v. Closing connection.\n", logPrefix, msg.Bytes(), err)
		// This leads to an error in .Decode(), terminating the handleIRC goroutine.
		ircConn.Close()
		return
	}
	log.Printf("%s ->irc: %q\n", logPrefix, msg.Bytes())
}

func handleIRC(conn net.Conn) {
	var (
		logPrefix = conn.RemoteAddr().String()
		ircConn   = irc.NewConn(conn)
		// TODO(secure): use proper prefix
		ircPrefix     = irc.Prefix{Name: "proxy.fancyirc"}
		ircErrors     = make(chan error)
		ircMessages   = make(chan irc.Message)
		fancyMessages = make(chan string)

		quitmsg  string
		done     bool
		pingSent bool
	)

	// TODO(secure): create a session first.

	go func() {
		for {
			message, err := ircConn.Decode()
			if err != nil {
				ircErrors <- err
				return
			}
			log.Printf("%s <-irc: %q\n", logPrefix, message.Bytes())
			ircMessages <- *message
		}
	}()

	// TODO(secure): periodically get all the servers in the network, overwrite allServers (so that deletions work)

	go func() {
		var lastSeen fancyId

		for !done {
			host := allServers[rand.Intn(len(allServers))]
			// TODO(secure): exponential backoff in a per-server fashion
			log.Printf("%s Connecting to %q...\n", logPrefix, host)
			// TODO(secure): build targets (= filters) and add them to the url
			hostUrl := fmt.Sprintf("http://%s/follow?lastseen=%d", host, lastSeen)
			resp, err := http.Get(hostUrl)
			if err != nil {
				log.Printf("%s HTTP GET %q failed: %v\n", logPrefix, hostUrl, err)
				continue
			}

			if resp.StatusCode != 200 {
				log.Printf("%s Received unexpected status code from %q: %v\n", logPrefix, host, resp.Status)
				continue
			}

			// We set the host as currentMaster, not because the host is the
			// master, but because it is reachable. When sending messages, we will
			// either reach the master by chance or get redirected, at which point
			// we update currentMaster.
			currentMaster = host

			dec := json.NewDecoder(resp.Body)
			for !done {
				// TODO(secure): we need a ping message here as well, so that we can detect timeouts quickly. It could include the current servers.
				var msg fancyMessage
				if err := dec.Decode(&msg); err != nil {
					log.Printf("%s Protocol error on %q: Could not decode response chunk as JSON: %v\n", logPrefix, host, err)
					break
				}

				log.Printf("%s <-fancy: %q\n", logPrefix, msg.Data)
				fancyMessages <- msg.Data
				lastSeen = msg.Id
			}
			resp.Body.Close()
		}

		close(fancyMessages)

		log.Printf("Disconnecting.\n")
	}()

	// Read all remaining messages to prevent goroutine hangs.
	defer func() {
		for _ = range fancyMessages {
		}

		log.Printf("TODO: close session with msg %q\n", quitmsg)
		// TODO: close the session on the server
	}()

	for {
		select {
		case <-time.After(1 * time.Minute):
			// After no traffic in either direction for 1 minute, we send a PING
			// message. If a PING message was already sent, this means that we did
			// not receive a PONG message, so we close the connection with at
			// timeout.
			if pingSent {
				quitmsg = "ping timeout"
				ircConn.Close()
			} else {
				sendIRCMessage(logPrefix, ircConn, irc.Message{
					Prefix:  &ircPrefix,
					Command: irc.PING,
					Params:  []string{"fancyirc.proxy"},
				})
			}

		case err := <-ircErrors:
			log.Printf("Error in IRC client connection: %v\n", err)
			done = true
			return

		case msg := <-fancyMessages:
			if _, err := fmt.Fprintf(conn, "%s\n", msg); err != nil {
				log.Fatal(err)
			}

		case message := <-ircMessages:
			switch message.Command {
			case irc.PONG:
				log.Printf("%s received PONG reply.\n", logPrefix)
				pingSent = false
			case irc.PING:
				sendIRCMessage(logPrefix, ircConn, irc.Message{
					Prefix:  &ircPrefix,
					Command: irc.PONG,
					Params:  message.Params,
				})
			case irc.QUIT:
				quitmsg = message.Trailing
			default:
				//case irc.NICK:
				//	log.Printf("requested nickname is %q\n", message.Params[0])
				//	nick = message.Params[0]
				//	// TODO(secure): this needs to create a session on the IRC server.
				//	// TODO(secure): figure out whether we want to have at most 1 irc connection per nickname/session.
				//case irc.USER:
				//	// TODO(secure): the irc server, not the proxy, is supposed to send these messages
				//	// TODO(secure): send 002, 003, 004, 005, 251, 252, 254, 255, 265, 266, [motd = 375, 372, 376]
				//	reply(irc.Message{
				//		Command:  irc.RPL_WELCOME,
				//		Params:   []string{nick},
				//		Trailing: "Welcome to fancyirc :)",
				//	})
				//case irc.PRIVMSG:
				// TODO: we need to associate a session with this.
				if err := sendFancyMessage(logPrefix, currentMaster, message.Bytes()); err != nil {
					// TODO(secure): what should we do here?
					log.Printf("message could not be sent: %v\n", err)
				}
			}
		}
	}
}

func main() {
	flag.Parse()

	rand.Seed(time.Now().Unix())

	// Start with any server. Will be overwritten later.
	allServers = strings.Split(*servers, ",")
	if len(allServers) == 0 {
		log.Fatalf("Invalid -servers value (%q). Need at least one server.\n", *servers)
	}
	currentMaster = allServers[0]

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}

	log.Printf("fancyirc proxy listening on %q\n", *listen)

	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("Could not accept IRC client connection: %v\n", err)
			continue
		}
		go handleIRC(conn)
	}
}
