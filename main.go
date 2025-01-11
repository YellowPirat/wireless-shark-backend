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

const (
	AF_CAN       = 29
	PF_CAN       = AF_CAN
	CAN_RAW      = 1
	SIOCGIFINDEX = 0x8933
)

type SockaddrCAN struct {
	Family  uint16
	Ifindex int32
	Addr    [14]byte
}

func (sa *SockaddrCAN) sockaddr() (unsafe.Pointer, _Socklen, error) {
	return unsafe.Pointer(sa), _Socklen(unsafe.Sizeof(*sa)), nil
}

type _Socklen uint32

type CANFrame struct {
	ID        uint32    `json:"id"`
	Length    uint8     `json:"length"`
	Data      [8]byte   `json:"data"`
	Timestamp time.Time `json:"timestamp"`
	SocketID  string    `json:"socket_id"`
}

type CANSocket struct {
	fd       int
	socketID string
	stop     chan struct{}
}

var (
	upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
	}
	clients    = make(map[*websocket.Conn]bool)
	clientsMux sync.RWMutex
	canSockets []*CANSocket
)

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket Upgrade Fehler: %v", err)
		return
	}
	defer conn.Close()

	clientsMux.Lock()
	clients[conn] = true
	clientsMux.Unlock()

	defer func() {
		clientsMux.Lock()
		delete(clients, conn)
		clientsMux.Unlock()
	}()

	for {
		_, _, err := conn.ReadMessage()
		if err != nil {
			break
		}
	}
}

func broadcastFrame(frame *CANFrame) {
	frameJSON, err := json.Marshal(frame)
	if err != nil {
		log.Printf("JSON Fehler: %v", err)
		return
	}

	clientsMux.RLock()
	defer clientsMux.RUnlock()

	for client := range clients {
		err := client.WriteMessage(websocket.TextMessage, frameJSON)
		if err != nil {
			log.Printf("Senden Fehler: %v", err)
			client.Close()
			delete(clients, client)
		}
	}
}

func startCANReader(socketID string, debug bool) (*CANSocket, error) {
	s, err := syscall.Socket(AF_CAN, syscall.SOCK_RAW, CAN_RAW)
	if err != nil {
		return nil, fmt.Errorf("Socket Erstellung fehlgeschlagen für %s: %v", socketID, err)
	}

	ifindex, err := getCANInterfaceIndex(socketID)
	if err != nil {
		syscall.Close(s)
		return nil, fmt.Errorf("Interface Index Abruf fehlgeschlagen für %s: %v", socketID, err)
	}

	addr := &SockaddrCAN{
		Family:  AF_CAN,
		Ifindex: int32(ifindex),
	}

	ptr, n, err := addr.sockaddr()
	if err != nil {
		syscall.Close(s)
		return nil, fmt.Errorf("Sockaddr Erstellung fehlgeschlagen für %s: %v", socketID, err)
	}

	_, _, errno := syscall.RawSyscall(syscall.SYS_BIND, uintptr(s), uintptr(ptr), uintptr(n))
	if errno != 0 {
		syscall.Close(s)
		return nil, fmt.Errorf("Bind fehlgeschlagen für %s: %v", socketID, errno)
	}

	canSocket := &CANSocket{
		fd:       s,
		socketID: socketID,
		stop:     make(chan struct{}),
	}

	go func() {
		for {
			select {
			case <-canSocket.stop:
				return
			default:
				frame := &CANFrame{SocketID: socketID}
				err := receiveCANFrame(s, frame)
				if err != nil {
					log.Printf("Frame-Empfang Fehler auf %s: %v", socketID, err)
					continue
				}
				frame.Timestamp = time.Now()
				broadcastFrame(frame)
				if debug{
					fmt.Printf("[%s] Frame: ID=%X, Len=%d, Data=%X, Time=%v\n",
						frame.SocketID, frame.ID, frame.Length, frame.Data, frame.Timestamp)
				}
			}
		}
	}()

	return canSocket, nil
}

func cleanup() {
	// Schließe alle CAN Sockets
	for _, socket := range canSockets {
		close(socket.stop)
		syscall.Close(socket.fd)
	}
}

func main() {
	// Definiere Kommandozeilenparameter
	interfaces := flag.String("interfaces", "", "Komma-separierte Liste von CAN-Interfaces (z.B. vcan0,vcan1,vcan2)")
	port := flag.String("port", "8080", "WebSocket Server Port")
	debug := flag.Bool("debug", False, "Debug Print aktivieren")
	flag.Parse()

	if *interfaces == "" {
		log.Fatal("Bitte geben Sie mindestens ein CAN-Interface an (-interfaces vcan0,vcan1,...)")
	}

	// Parse die Interface-Liste
	canInterfaceList := strings.Split(*interfaces, ",")

	// Entferne eventuelle Leerzeichen
	for i, iface := range canInterfaceList {
		canInterfaceList[i] = strings.TrimSpace(iface)
	}

	log.Printf("Starte mit folgenden CAN-Interfaces: %v", canInterfaceList)

	// Starte CAN Reader für jedes Interface
	for _, iface := range canInterfaceList {
		canSocket, err := startCANReader(iface, *debug)
		if err != nil {
			log.Printf("Fehler beim Starten des CAN Readers für %s: %v", iface, err)
			cleanup()
			log.Fatal(err)
		}
		canSockets = append(canSockets, canSocket)
		log.Printf("CAN Reader gestartet für Interface: %s", iface)
	}

	// Cleanup bei Programmende
	defer cleanup()

	http.HandleFunc("/ws", handleWebSocket)
	http.Handle("/", http.FileServer(http.Dir("./static")))

	serverAddr := fmt.Sprintf(":%s", *port)
	log.Printf("Server läuft auf http://localhost%s", serverAddr)
	log.Fatal(http.ListenAndServe(serverAddr, nil))
}

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

func receiveCANFrame(s int, frame *CANFrame) error {
	frameBytes := make([]byte, 16)
	n, err := syscall.Read(s, frameBytes)
	if err != nil {
		return err
	}
	if n != 16 {
		return fmt.Errorf("Unerwartete Frame-Größe: %d", n)
	}

	frame.ID = binary.LittleEndian.Uint32(frameBytes[0:4])
	frame.Length = frameBytes[4]
	copy(frame.Data[:frame.Length], frameBytes[8:8+frame.Length])

	for i := frame.Length; i < 8; i++ {
		frame.Data[i] = 0
	}

	return nil
}
