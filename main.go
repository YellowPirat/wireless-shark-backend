package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
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
	NUM_CAN      = 6 // Anzahl der CAN-Interfaces
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
	SocketID  string    `json:"socket_id"` // Neue Feld für Socket-Identifikation
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

var clients = make(map[*websocket.Conn]bool)

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket Upgrade Fehler: %v", err)
		return
	}
	defer conn.Close()

	clients[conn] = true
	defer delete(clients, conn)

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

	for client := range clients {
		err := client.WriteMessage(websocket.TextMessage, frameJSON)
		if err != nil {
			log.Printf("Senden Fehler: %v", err)
			client.Close()
			delete(clients, client)
		}
	}
}

func startCANReader(socketID string) error {
	s, err := syscall.Socket(AF_CAN, syscall.SOCK_RAW, CAN_RAW)
	if err != nil {
		return fmt.Errorf("Socket Erstellung fehlgeschlagen für %s: %v", socketID, err)
	}
	defer syscall.Close(s)

	ifindex, err := getCANInterfaceIndex(socketID)
	if err != nil {
		return fmt.Errorf("Interface Index Abruf fehlgeschlagen für %s: %v", socketID, err)
	}

	addr := &SockaddrCAN{
		Family:  AF_CAN,
		Ifindex: int32(ifindex),
	}

	ptr, n, err := addr.sockaddr()
	if err != nil {
		return fmt.Errorf("Sockaddr Erstellung fehlgeschlagen für %s: %v", socketID, err)
	}

	r1, r2, errno := syscall.RawSyscall(syscall.SYS_BIND, uintptr(s), uintptr(ptr), uintptr(n))
	if errno != 0 {
		return fmt.Errorf("Bind fehlgeschlagen für %s: %v", socketID, errno)
	}
	_ = r1
	_ = r2

	go func() {
		for {
			frame := &CANFrame{SocketID: socketID}
			err := receiveCANFrame(s, frame)
			if err != nil {
				log.Printf("Frame-Empfang Fehler auf %s: %v", socketID, err)
				continue
			}
			frame.Timestamp = time.Now()
			broadcastFrame(frame)
			fmt.Printf("[%s] Frame: ID=%X, Len=%d, Data=%X, Time=%v\n",
				frame.SocketID, frame.ID, frame.Length, frame.Data, frame.Timestamp)
		}
	}()

	return nil
}

func main() {
	// Starte CAN Reader für jedes Interface
	canInterfaces := []string{"vcan0", "vcan1", "vcan2", "vcan3", "vcan4", "vcan5"}

	for _, iface := range canInterfaces {
		err := startCANReader(iface)
		if err != nil {
			log.Printf("Fehler beim Starten des CAN Readers für %s: %v", iface, err)
			continue
		}
		log.Printf("CAN Reader gestartet für Interface: %s", iface)
	}

	http.HandleFunc("/ws", handleWebSocket)
	http.Handle("/", http.FileServer(http.Dir("./static")))

	log.Println("Server läuft auf http://localhost:8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
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
