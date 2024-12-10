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

func main() {
	s, err := syscall.Socket(AF_CAN, syscall.SOCK_RAW, CAN_RAW)
	if err != nil {
		log.Fatal(err)
	}
	defer syscall.Close(s)

	ifindex, err := getCANInterfaceIndex("vcan0")
	if err != nil {
		log.Fatal(err)
	}

	addr := &SockaddrCAN{
		Family:  AF_CAN,
		Ifindex: int32(ifindex),
	}

	ptr, n, err := addr.sockaddr()
	if err != nil {
		log.Fatal(err)
	}
	r1, r2, errno := syscall.RawSyscall(syscall.SYS_BIND, uintptr(s), uintptr(ptr), uintptr(n))
	if errno != 0 {
		log.Fatal(errno)
	}
	_ = r1
	_ = r2

	go func() {
		for {
			frame := &CANFrame{}
			err := receiveCANFrame(s, frame)
			if err != nil {
				log.Printf("Frame-Empfang Fehler: %v", err)
				continue
			}
			// Zeitstempel setzen
			frame.Timestamp = time.Now()
			broadcastFrame(frame)
			fmt.Printf("Frame: ID=%X, Len=%d, Data=%X, Time=%v\n",
				frame.ID, frame.Length, frame.Data, frame.Timestamp)
		}
	}()

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

	// Die CAN ID ist in den ersten 4 Bytes in Little Endian
	frame.ID = binary.LittleEndian.Uint32(frameBytes[0:4])

	// Die Länge ist im 5. Byte
	frame.Length = frameBytes[4]

	// Nur die tatsächliche Datenlänge kopieren, nicht alle 8 Bytes
	copy(frame.Data[:frame.Length], frameBytes[8:8+frame.Length])

	// Rest mit 0 füllen
	for i := frame.Length; i < 8; i++ {
		frame.Data[i] = 0
	}

	return nil
}
