package tcp

import (
	"bytes"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	log "github.com/schollz/logger"
	"github.com/schollz/pake/v3"

	"github.com/schollz/croc/v10/src/comm"
	"github.com/schollz/croc/v10/src/crypt"
	"github.com/schollz/croc/v10/src/models"
)

type server struct {
	host       string
	port       string
	debugLevel string
	banner     string
	password   string
	rooms      roomMap

	roomCleanupInterval time.Duration
	roomTTL             time.Duration

	stopRoomCleanup chan struct{}
}

type roomInfo struct {
	first  *comm.Comm
	second *comm.Comm
	opened time.Time
	full   bool
}

type roomMap struct {
	rooms map[string]roomInfo
	sync.Mutex
}

const pingRoom = "pinglkasjdlfjsaldjf"

// newDefaultServer initializes a new server, with some default configuration options
func newDefaultServer() *server {
	fmt.Println("Initializing new default server")
	s := new(server)
	s.roomCleanupInterval = DEFAULT_ROOM_CLEANUP_INTERVAL
	s.roomTTL = DEFAULT_ROOM_TTL
	s.debugLevel = DEFAULT_LOG_LEVEL
	s.stopRoomCleanup = make(chan struct{})
	fmt.Println("New default server initialized")
	return s
}

// RunWithOptionsAsync asynchronously starts a TCP listener.
func RunWithOptionsAsync(host, port, password string, opts ...serverOptsFunc) error {
	fmt.Printf("Running with options async: host=%s, port=%s\n", host, port)
	s := newDefaultServer()
	s.host = host
	s.port = port
	s.password = password
	for _, opt := range opts {
		fmt.Println("Applying server option")
		err := opt(s)
		if err != nil {
			fmt.Println("Error applying server option")
			return fmt.Errorf("could not apply optional configurations: %w", err)
		}
	}
	fmt.Println("All options applied, starting server")
	return s.start()
}

// Run starts a tcp listener, run async
func Run(debugLevel, host, port, password string, banner ...string) (err error) {
	fmt.Printf("Run called with debugLevel=%s, host=%s, port=%s\n", debugLevel, host, port)
	return RunWithOptionsAsync(host, port, password, WithBanner(banner...), WithLogLevel(debugLevel))
}

func (s *server) start() (err error) {
	fmt.Println("Starting server")
	log.SetLevel(s.debugLevel)

	maskedPassword := ""
	if len(s.password) > 2 {
		maskedPassword = fmt.Sprintf("%c***%c", s.password[0], s.password[len(s.password)-1])
	} else {
		maskedPassword = s.password
	}

	log.Debugf("starting with password '%s'", maskedPassword)

	s.rooms.Lock()
	s.rooms.rooms = make(map[string]roomInfo)
	s.rooms.Unlock()

	go s.deleteOldRooms()
	defer s.stopRoomDeletion()

	fmt.Println("Running server now")
	err = s.run()
	if err != nil {
		log.Error(err)
	}
	fmt.Println("Server start completed")
	return
}

func (s *server) run() (err error) {
	fmt.Println("Running main server loop")
	network := "tcp"
	addr := net.JoinHostPort(s.host, s.port)
	if s.host != "" {
		ip := net.ParseIP(s.host)
		if ip == nil {
			fmt.Println("Resolving host IP")
			var tcpIP *net.IPAddr
			tcpIP, err = net.ResolveIPAddr("ip", s.host)
			if err != nil {
				fmt.Println("Error resolving IP")
				return err
			}
			ip = tcpIP.IP
		}
		addr = net.JoinHostPort(ip.String(), s.port)
		if s.host != "" {
			if ip.To4() != nil {
				network = "tcp4"
			} else {
				network = "tcp6"
			}
		}
	}
	addr = strings.Replace(addr, "127.0.0.1", "0.0.0.0", 1)
	log.Infof("starting TCP server on " + addr)
	fmt.Printf("Listening on %s\n", addr)
	server, err := net.Listen(network, addr)
	if err != nil {
		fmt.Println("Error listening on the server")
		return fmt.Errorf("error listening on %s: %w", addr, err)
	}
	defer server.Close()
	for {
		fmt.Println("Waiting for client connections...")
		connection, err := server.Accept()
		if err != nil {
			fmt.Println("Error accepting connection")
			return fmt.Errorf("problem accepting connection: %w", err)
		}
		log.Debugf("client %s connected", connection.RemoteAddr().String())
		fmt.Printf("Client connected: %s\n", connection.RemoteAddr().String())
		go func(port string, connection net.Conn) {
			fmt.Println("Starting client communication")
			c := comm.New(connection)
			room, errCommunication := s.clientCommunication(port, c)
			fmt.Printf("Room after client communication: %s\n", room)
			fmt.Printf("Error from client communication: %v\n", errCommunication)
			if errCommunication != nil {
				log.Debugf("relay-%s: %s", connection.RemoteAddr().String(), errCommunication.Error())
				fmt.Println("Closing connection due to communication error")
				connection.Close()
				return
			}
			if room == pingRoom {
				log.Debugf("got ping")
				fmt.Println("Got ping, closing connection")
				connection.Close()
				return
			}
			for {
				fmt.Printf("Checking connection for room: %s\n", room)
				deleteIt := false
				s.rooms.Lock()
				if _, ok := s.rooms.rooms[room]; !ok {
					fmt.Println("Room no longer exists")
					s.rooms.Unlock()
					return
				}
				fmt.Printf("Current room info: %+v\n", s.rooms.rooms[room])
				if s.rooms.rooms[room].first != nil && s.rooms.rooms[room].second != nil {
					fmt.Println("Room connections ready")
					s.rooms.Unlock()
					break
				} else {
					if s.rooms.rooms[room].first != nil {
						errSend := s.rooms.rooms[room].first.Send([]byte{1})
						if errSend != nil {
							fmt.Println("Error sending keepalive")
							log.Debug(errSend)
							deleteIt = true
						}
					}
				}
				s.rooms.Unlock()
				if deleteIt {
					fmt.Println("Deleting room due to error")
					s.deleteRoom(room)
					break
				}
				fmt.Println("Sleeping before next connection check")
				time.Sleep(1 * time.Second)
			}
			fmt.Println("Done checking connections, client goroutine ends")
		}(s.port, connection)
	}
}

func (s *server) deleteOldRooms() {
	fmt.Println("Starting room cleanup goroutine")
	ticker := time.NewTicker(s.roomCleanupInterval)
	for {
		select {
		case <-ticker.C:
			fmt.Println("Checking for old rooms to delete")
			var roomsToDelete []string
			s.rooms.Lock()
			for room := range s.rooms.rooms {
				if time.Since(s.rooms.rooms[room].opened) > s.roomTTL {
					fmt.Printf("Room %s is old, marking for deletion\n", room)
					roomsToDelete = append(roomsToDelete, room)
				}
			}
			s.rooms.Unlock()

			for _, room := range roomsToDelete {
				fmt.Printf("Deleting old room: %s\n", room)
				s.deleteRoom(room)
				log.Debugf("room cleaned up: %s", room)
			}
		case <-s.stopRoomCleanup:
			fmt.Println("Stopping room cleanup ticker")
			ticker.Stop()
			log.Debug("room cleanup stopped")
			return
		}
	}
}

func (s *server) stopRoomDeletion() {
	fmt.Println("Stop room cleanup fired")
	s.stopRoomCleanup <- struct{}{}
}

var weakKey = []byte{1, 2, 3}

func (s *server) clientCommunication(port string, c *comm.Comm) (room string, err error) {
	fmt.Println("Starting clientCommunication")
	B, err := pake.InitCurve(weakKey, 1, "siec")
	if err != nil {
		fmt.Println("Error initializing PAKE B")
		return
	}
	Abytes, err := c.Receive()
	if err != nil {
		fmt.Println("Error receiving Abytes")
		return
	}
	log.Debugf("Abytes: %s", Abytes)
	if bytes.Equal(Abytes, []byte("ping")) {
		room = pingRoom
		log.Debug("sending back pong")
		fmt.Println("Received ping, sending pong")
		c.Send([]byte("pong"))
		return
	}
	err = B.Update(Abytes)
	if err != nil {
		fmt.Println("Error updating B with Abytes")
		return
	}
	err = c.Send(B.Bytes())
	if err != nil {
		fmt.Println("Error sending B bytes")
		return
	}
	strongKey, err := B.SessionKey()
	if err != nil {
		fmt.Println("Error getting session key")
		return
	}
	log.Debugf("strongkey: %x", strongKey)
	fmt.Println("Got strong key")

	salt, err := c.Receive()
	if err != nil {
		fmt.Println("Error receiving salt")
		return
	}
	fmt.Println("Received salt")
	strongKeyForEncryption, _, err := crypt.New(strongKey, salt)
	if err != nil {
		fmt.Println("Error creating encryption key from strongKey")
		return
	}

	fmt.Println("Waiting for password")
	passwordBytesEnc, err := c.Receive()
	if err != nil {
		fmt.Println("Error receiving encrypted password")
		return
	}
	passwordBytes, err := crypt.Decrypt(passwordBytesEnc, strongKeyForEncryption)
	if err != nil {
		fmt.Println("Error decrypting password")
		return
	}
	fmt.Printf("Received password: %s\n", string(passwordBytes))
	if strings.TrimSpace(string(passwordBytes)) != s.password {
		fmt.Println("Password mismatch")
		err = fmt.Errorf("bad password")
		enc, _ := crypt.Encrypt([]byte(err.Error()), strongKeyForEncryption)
		if err = c.Send(enc); err != nil {
			fmt.Println("Error sending bad password response")
			return "", fmt.Errorf("send error: %w", err)
		}
		return
	}

	banner := s.banner
	if len(banner) == 0 {
		banner = "ok"
	}
	log.Debugf("sending '%s'", banner)
	bSend, err := crypt.Encrypt([]byte(banner+"|||"+c.Connection().RemoteAddr().String()), strongKeyForEncryption)
	if err != nil {
		fmt.Println("Error encrypting banner")
		return
	}
	err = c.Send(bSend)
	if err != nil {
		fmt.Println("Error sending banner")
		return
	}
	fmt.Println("Banner sent successfully")

	fmt.Println("Waiting for room")
	enc, err := c.Receive()
	if err != nil {
		fmt.Println("Error receiving room request")
		return
	}
	roomBytes, err := crypt.Decrypt(enc, strongKeyForEncryption)
	if err != nil {
		fmt.Println("Error decrypting room name")
		return
	}
	room = string(roomBytes)
	fmt.Printf("Requested room: %s\n", room)

	s.rooms.Lock()
	if _, ok := s.rooms.rooms[room]; !ok {
		fmt.Println("Creating new room")
		s.rooms.rooms[room] = roomInfo{
			first:  c,
			opened: time.Now(),
		}
		s.rooms.Unlock()
		bSend, err = crypt.Encrypt([]byte("ok"), strongKeyForEncryption)
		if err != nil {
			fmt.Println("Error encrypting ok message")
			return
		}
		err = c.Send(bSend)
		if err != nil {
			fmt.Println("Error sending ok to first room client")
			log.Error(err)
			s.deleteRoom(room)
			return
		}
		fmt.Println("Room created and ok sent to first client")
		log.Debugf("room %s has 1", room)
		return
	}
	if s.rooms.rooms[room].full {
		fmt.Println("Room is already full")
		s.rooms.Unlock()
		bSend, err = crypt.Encrypt([]byte("room full"), strongKeyForEncryption)
		if err != nil {
			fmt.Println("Error encrypting room full message")
			return
		}
		err = c.Send(bSend)
		if err != nil {
			fmt.Println("Error sending room full message")
			log.Error(err)
			return
		}
		fmt.Println("Notified client that room is full")
		return
	}
	fmt.Println("Joining existing room as second client")
	s.rooms.rooms[room] = roomInfo{
		first:  s.rooms.rooms[room].first,
		second: c,
		opened: s.rooms.rooms[room].opened,
		full:   true,
	}
	otherConnection := s.rooms.rooms[room].first
	s.rooms.Unlock()

	fmt.Println("Starting piping between two clients")
	var wg sync.WaitGroup
	wg.Add(1)

	go func(com1, com2 *comm.Comm, wg *sync.WaitGroup) {
		fmt.Println("Piping goroutine started")
		log.Debug("starting pipes")
		pipe(com1.Connection(), com2.Connection())
		wg.Done()
		log.Debug("done piping")
		fmt.Println("Piping goroutine finished")
	}(otherConnection, c, &wg)

	bSend, err = crypt.Encrypt([]byte("ok"), strongKeyForEncryption)
	if err != nil {
		fmt.Println("Error encrypting final ok message")
		return
	}
	err = c.Send(bSend)
	if err != nil {
		fmt.Println("Error sending final ok message")
		s.deleteRoom(room)
		return
	}
	fmt.Println("Final ok sent to second client, waiting for piping to finish")
	wg.Wait()

	fmt.Println("Piping done, deleting room")
	s.deleteRoom(room)
	return
}

func (s *server) deleteRoom(room string) {
	fmt.Printf("Deleting room: %s\n", room)
	s.rooms.Lock()
	defer s.rooms.Unlock()
	if _, ok := s.rooms.rooms[room]; !ok {
		fmt.Println("Room not found, nothing to delete")
		return
	}
	log.Debugf("deleting room: %s", room)
	if s.rooms.rooms[room].first != nil {
		fmt.Println("Closing first connection")
		s.rooms.rooms[room].first.Close()
	}
	if s.rooms.rooms[room].second != nil {
		fmt.Println("Closing second connection")
		s.rooms.rooms[room].second.Close()
	}
	s.rooms.rooms[room] = roomInfo{first: nil, second: nil}
	delete(s.rooms.rooms, room)
	fmt.Println("Room deleted successfully")
}

func chanFromConn(conn net.Conn) chan []byte {
	fmt.Println("Creating channel from connection")
	c := make(chan []byte, 1)
	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Hour)); err != nil {
		fmt.Println("Error setting read deadline")
		log.Warnf("can't set read deadline: %v", err)
	}

	go func() {
		b := make([]byte, models.TCP_BUFFER_SIZE)
		for {
			n, err := conn.Read(b)
			if n > 0 {
				res := make([]byte, n)
				copy(res, b[:n])
				c <- res
			}
			if err != nil {
				fmt.Println("Error reading from connection or EOF reached")
				log.Debug(err)
				c <- nil
				break
			}
		}
		log.Debug("exiting")
		fmt.Println("Exiting channel goroutine")
	}()

	return c
}

func pipe(conn1 net.Conn, conn2 net.Conn) {
	fmt.Println("Starting pipe function")
	chan1 := chanFromConn(conn1)
	chan2 := chanFromConn(conn2)

	for {
		select {
		case b1 := <-chan1:
			if b1 == nil {
				fmt.Println("Channel 1 closed, returning")
				return
			}
			_, err := conn2.Write(b1)
			if err != nil {
				fmt.Println("Error writing to conn2")
				log.Errorf("write error on channel 1: %v", err)
			}

		case b2 := <-chan2:
			if b2 == nil {
				fmt.Println("Channel 2 closed, returning")
				return
			}
			_, err := conn1.Write(b2)
			if err != nil {
				fmt.Println("Error writing to conn1")
				log.Errorf("write error on channel 2: %v", err)
			}
		}
	}
}

func PingServer(address string) (err error) {
	fmt.Printf("Pinging server: %s\n", address)
	log.Debugf("pinging %s", address)
	c, err := comm.NewConnection(address, 300*time.Millisecond)
	if err != nil {
		fmt.Println("Error creating new connection for ping")
		log.Debug(err)
		return
	}
	err = c.Send([]byte("ping"))
	if err != nil {
		fmt.Println("Error sending ping")
		log.Debug(err)
		return
	}
	b, err := c.Receive()
	if err != nil {
		fmt.Println("Error receiving pong")
		log.Debug(err)
		return
	}
	if bytes.Equal(b, []byte("pong")) {
		fmt.Println("Ping successful, got pong")
		return nil
	}
	fmt.Println("No pong received")
	return fmt.Errorf("no pong")
}

func ConnectToTCPServer(address, password, room string, timelimit ...time.Duration) (c *comm.Comm, banner string, ipaddr string, err error) {
	fmt.Printf("Connecting to TCP Server: address=%s, room=%s\n", address, room)
	if len(timelimit) > 0 {
		fmt.Printf("Timelimit specified: %s\n", timelimit[0])
		c, err = comm.NewConnection(address, timelimit[0])
	} else {
		c, err = comm.NewConnection(address)
	}
	if err != nil {
		fmt.Println("Error establishing new connection")
		log.Debug(err)
		return
	}

	A, err := pake.InitCurve(weakKey, 0, "siec")
	if err != nil {
		fmt.Println("Error initializing PAKE A")
		log.Debug(err)
		return
	}
	err = c.Send(A.Bytes())
	if err != nil {
		fmt.Println("Error sending A bytes")
		log.Debug(err)
		return
	}
	Bbytes, err := c.Receive()
	if err != nil {
		fmt.Println("Error receiving B bytes")
		log.Debug(err)
		return
	}
	err = A.Update(Bbytes)
	if err != nil {
		fmt.Println("Error updating A with Bbytes")
		log.Debug(err)
		return
	}
	strongKey, err := A.SessionKey()
	if err != nil {
		fmt.Println("Error getting session key from A")
		log.Debug(err)
		return
	}
	log.Debugf("strong key: %x", strongKey)
	fmt.Println("Got strong key on client side")

	strongKeyForEncryption, salt, err := crypt.New(strongKey, nil)
	if err != nil {
		fmt.Println("Error creating new encryption from strongKey on client side")
		log.Debug(err)
		return
	}
	err = c.Send(salt)
	if err != nil {
		fmt.Println("Error sending salt from client side")
		log.Debug(err)
		return
	}

	fmt.Println("Sending password to server")
	bSend, err := crypt.Encrypt([]byte(password), strongKeyForEncryption)
	if err != nil {
		fmt.Println("Error encrypting password")
		log.Debug(err)
		return
	}
	err = c.Send(bSend)
	if err != nil {
		fmt.Println("Error sending encrypted password")
		log.Debug(err)
		return
	}
	fmt.Println("Waiting for server banner")
	enc, err := c.Receive()
	if err != nil {
		fmt.Println("Error receiving server banner")
		log.Debug(err)
		return
	}
	data, err := crypt.Decrypt(enc, strongKeyForEncryption)
	if err != nil {
		fmt.Println("Error decrypting server banner")
		log.Debug(err)
		return
	}
	if !strings.Contains(string(data), "|||") {
		fmt.Println("Bad response from server, no banner")
		err = fmt.Errorf("bad response: %s", string(data))
		log.Debug(err)
		return
	}
	banner = strings.Split(string(data), "|||")[0]
	ipaddr = strings.Split(string(data), "|||")[1]
	fmt.Printf("Received banner: %s, ipaddr: %s\n", banner, ipaddr)

	fmt.Printf("Sending room request: %s\n", room)
	bSend, err = crypt.Encrypt([]byte(room), strongKeyForEncryption)
	if err != nil {
		fmt.Println("Error encrypting room request")
		log.Debug(err)
		return
	}
	err = c.Send(bSend)
	if err != nil {
		fmt.Println("Error sending room request")
		log.Debug(err)
		return
	}
	fmt.Println("Waiting for room confirmation")
	enc, err = c.Receive()
	if err != nil {
		fmt.Println("Error receiving room confirmation")
		log.Debug(err)
		return
	}
	data, err = crypt.Decrypt(enc, strongKeyForEncryption)
	if err != nil {
		fmt.Println("Error decrypting room confirmation")
		log.Debug(err)
		return
	}
	if !bytes.Equal(data, []byte("ok")) {
		fmt.Println("Got bad response instead of ok")
		err = fmt.Errorf("got bad response: %s", data)
		log.Debug(err)
		return
	}
	fmt.Println("Room confirmed, all set")
	return
}
