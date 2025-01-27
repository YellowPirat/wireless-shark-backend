package main

import (
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/gorilla/websocket"
)

// CAN protocol specific constants
const (
	AF_CAN       = 29     // Address family for CAN
	PF_CAN       = AF_CAN // Protocol family for CAN
	CAN_RAW      = 1      // Raw CAN protocol
	SIOCGIFINDEX = 0x8933 // IOCTL command to get interface index
)

// SockaddrCAN represents the CAN socket address structure
type SockaddrCAN struct {
	Family  uint16   // Protocol family (AF_CAN)
	Ifindex int32    // Interface index
	Addr    [14]byte // Optional address information
}

// sockaddr converts SockaddrCAN to a system socket address
func (sa *SockaddrCAN) sockaddr() (unsafe.Pointer, _Socklen, error) {
	return unsafe.Pointer(sa), _Socklen(unsafe.Sizeof(*sa)), nil
}

type _Socklen uint32

// CANFrame represents a single CAN frame with metadata
type CANFrame struct {
	ID        uint32    `json:"id"`        // Frame identifier
	Length    uint8     `json:"length"`    // Data length
	Data      [8]byte   `json:"data"`      // Frame payload (max 8 bytes)
	Timestamp time.Time `json:"timestamp"` // Reception timestamp
	SocketID  string    `json:"socket_id"` // Source interface identifier
}

// CANSocket represents a single CAN interface connection
type CANSocket struct {
	fd       int           // File descriptor
	socketID string        // Interface identifier
	stop     chan struct{} // Channel for graceful shutdown
}

var (
	// WebSocket configuration and client management
	upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return true // Allow all origins for development
		},
	}
	clients    = make(map[*websocket.Conn]bool) // Active WebSocket clients
	clientsMux sync.RWMutex                     // Mutex for thread-safe client operations
	canSockets []*CANSocket                     // Active CAN sockets
)

// handleWebSocket manages WebSocket connections and client lifecycle
func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade error: %v", err)
		return
	}
	defer conn.Close()

	// Register new client
	clientsMux.Lock()
	clients[conn] = true
	clientsMux.Unlock()

	// Cleanup on connection close
	defer func() {
		clientsMux.Lock()
		delete(clients, conn)
		clientsMux.Unlock()
	}()

	// Keep connection alive until client disconnects
	for {
		_, _, err := conn.ReadMessage()
		if err != nil {
			break
		}
	}
}

// broadcastFrame sends a CAN frame to all connected WebSocket clients
func broadcastFrame(frame *CANFrame) {
	frameJSON, err := json.Marshal(frame)
	if err != nil {
		log.Printf("JSON marshaling error: %v", err)
		return
	}

	clientsMux.RLock()
	defer clientsMux.RUnlock()

	for client := range clients {
		err := client.WriteMessage(websocket.TextMessage, frameJSON)
		if err != nil {
			log.Printf("Client send error: %v", err)
			client.Close()
			delete(clients, client)
		}
	}
}

// startCANReader initializes and starts a CAN interface reader
func startCANReader(socketID string, debug bool) (*CANSocket, error) {
	// Create raw CAN socket
	s, err := syscall.Socket(AF_CAN, syscall.SOCK_RAW, CAN_RAW)
	if err != nil {
		return nil, fmt.Errorf("failed to create socket for %s: %v", socketID, err)
	}

	// Get interface index
	ifindex, err := getCANInterfaceIndex(socketID)
	if err != nil {
		syscall.Close(s)
		return nil, fmt.Errorf("failed to get interface index for %s: %v", socketID, err)
	}

	// Prepare socket address
	addr := &SockaddrCAN{
		Family:  AF_CAN,
		Ifindex: int32(ifindex),
	}

	ptr, n, err := addr.sockaddr()
	if err != nil {
		syscall.Close(s)
		return nil, fmt.Errorf("failed to create sockaddr for %s: %v", socketID, err)
	}

	// Bind socket to interface
	_, _, errno := syscall.RawSyscall(syscall.SYS_BIND, uintptr(s), uintptr(ptr), uintptr(n))
	if errno != 0 {
		syscall.Close(s)
		return nil, fmt.Errorf("failed to bind socket for %s: %v", socketID, errno)
	}

	canSocket := &CANSocket{
		fd:       s,
		socketID: socketID,
		stop:     make(chan struct{}),
	}

	// Start frame reading goroutine
	go func() {
		for {
			select {
			case <-canSocket.stop:
				return
			default:
				frame := &CANFrame{SocketID: socketID}
				err := receiveCANFrame(s, frame)
				if err != nil {
					log.Printf("Frame reception error on %s: %v", socketID, err)
					continue
				}
				frame.Timestamp = time.Now()
				broadcastFrame(frame)
				if debug {
					fmt.Printf("[%s] Frame: ID=%X, Len=%d, Data=%X, Time=%v\n",
						frame.SocketID, frame.ID, frame.Length, frame.Data, frame.Timestamp)
				}
			}
		}
	}()

	return canSocket, nil
}

// cleanup performs graceful shutdown of all CAN sockets
func cleanup() {
	for _, socket := range canSockets {
		close(socket.stop)
		syscall.Close(socket.fd)
	}
}

func main() {
	// Parse command line arguments
	interfaces := flag.String("interfaces", "", "Comma-separated list of CAN interfaces (e.g., vcan0,vcan1,vcan2)")
	port := flag.String("port", "8080", "WebSocket server port")
	debug := flag.Bool("debug", false, "Enable debug output")
	flag.Parse()

	if *interfaces == "" {
		log.Fatal("Please specify at least one CAN interface (-interfaces vcan0,vcan1,...)")
	}

	// Parse interface list
	canInterfaceList := strings.Split(*interfaces, ",")

	// Trim whitespace from interface names
	for i, iface := range canInterfaceList {
		canInterfaceList[i] = strings.TrimSpace(iface)
	}

	log.Printf("Starting with CAN interfaces: %v", canInterfaceList)

	// Initialize CAN readers
	for _, iface := range canInterfaceList {
		canSocket, err := startCANReader(iface, *debug)
		if err != nil {
			log.Printf("Error starting CAN reader for %s: %v", iface, err)
			cleanup()
			log.Fatal(err)
		}
		canSockets = append(canSockets, canSocket)
		log.Printf("CAN reader started for interface: %s", iface)
	}

	// Register cleanup handler
	defer cleanup()

	// Setup HTTP routes
	http.HandleFunc("/ws", handleWebSocket)
	http.Handle("/", http.FileServer(http.Dir("./static")))

	// Start server
	serverAddr := fmt.Sprintf(":%s", *port)
	log.Printf("Server running at http://localhost%s", serverAddr)
	log.Fatal(http.ListenAndServe(serverAddr, nil))
}

// getCANInterfaceIndex retrieves the system interface index for a CAN interface
func getCANInterfaceIndex(ifname string) (int, error) {
	ifreq := struct {
		Name  [16]byte
		Index int32
		_pad  [20]byte
	}{}
	copy(ifreq.Name[:], ifname)

	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, 0)
	if err != nil {
		return 0, err
	}
	defer syscall.Close(fd)

	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL,
		uintptr(fd),
		SIOCGIFINDEX,
		uintptr(unsafe.Pointer(&ifreq)))

	if errno != 0 {
		return 0, errno
	}

	return int(ifreq.Index), nil
}

// receiveCANFrame reads a single CAN frame from the socket
func receiveCANFrame(s int, frame *CANFrame) error {
	frameBytes := make([]byte, 16)
	n, err := syscall.Read(s, frameBytes)
	if err != nil {
		return err
	}
	if n != 16 {
		return fmt.Errorf("unexpected frame size: %d", n)
	}

	// Parse frame data
	frame.ID = binary.LittleEndian.Uint32(frameBytes[0:4])
	frame.Length = frameBytes[4]
	copy(frame.Data[:frame.Length], frameBytes[8:8+frame.Length])

	// Zero-pad remaining data bytes
	for i := frame.Length; i < 8; i++ {
		frame.Data[i] = 0
	}

	return nil
}
