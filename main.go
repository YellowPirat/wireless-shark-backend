package main

import (
	"encoding/json"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"strings"
	"sync"
	"syscall"
	"os"
	"os/exec"
	"path/filepath"
	"log"
	"time"
	"unsafe"
	"github.com/gorilla/websocket"
	"strconv"
)

// constants
const (
	uploadDir       = "/var/www"
	assignmentsFile = "/var/www/assignments/can_assignments.json"
	AF_CAN       = 29     // Address family for CAN
	PF_CAN       = AF_CAN // Protocol family for CAN
	CAN_RAW      = 1      // Raw CAN protocol
	SIOCGIFINDEX = 0x8933 // IOCTL command to get interface index
)

// variables
var (
	loggerCmd *exec.Cmd
	mutex     sync.Mutex
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

// structs
type Response struct {
	Message string `json:"message"`
}

// SockaddrCAN represents the CAN socket address structure
type SockaddrCAN struct {
	Family  uint16   // Protocol family (AF_CAN)
	Ifindex int32    // Interface index
	Addr    [14]byte // Optional address information
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

type CANAssignment struct {
	CANSocket string `json:"CANSocket"`
	DBCFile   string `json:"DBCFile"`
	YAMLFile  string `json:"YAMLFile"`
}

func (sa *SockaddrCAN) sockaddr() (unsafe.Pointer, _Socklen, error) {
	return unsafe.Pointer(sa), _Socklen(unsafe.Sizeof(*sa)), nil
}

func corsMiddleware(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        // setting CORS-header 
        w.Header().Set("Access-Control-Allow-Origin", "*")  // allow request from every origin
        w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS, DELETE")    // allow GET, POST and OPTIONS
        w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")  // allow specific header
        w.Header().Set("Access-Control-Allow-Credentials", "true")  // allow cookies "login credentials"

        if r.Method == "OPTIONS" {
            w.WriteHeader(http.StatusOK)
            return
        }

        next.ServeHTTP(w, r)
    })
}

func listFiles(w http.ResponseWriter, r *http.Request) {
    files, err := os.ReadDir("/var/www")
    if err != nil {
        http.Error(w, "Failed to read files", http.StatusInternalServerError)
		log.Println("Lesen des Verzeichnisses fehlgeschlagen")
        return
    }

    var fileNames []string
    for _, file := range files {
        if !file.IsDir() {
            fileNames = append(fileNames, file.Name())
        }
    }

    w.Header().Set("Content-Type", "application/json")
    json.NewEncoder(w).Encode(fileNames)
	log.Println("Reading directory successfully")
}

func startLogger(w http.ResponseWriter, r *http.Request) {
	mutex.Lock()
	defer mutex.Unlock()

	if loggerCmd != nil && loggerCmd.Process != nil {
		http.Error(w, "Logger is already running", http.StatusConflict)
		log.Println("Logger is already running")
		return
	}

	yamlFile := r.URL.Query().Get("yaml")
	if yamlFile == "" {
		http.Error(w, "Missing yaml file parameter", http.StatusBadRequest)
		log.Println("Missing yaml file parameter")
		return
	}

	yamlPath := filepath.Join(uploadDir, yamlFile)
	if _, err := os.Stat(yamlPath); os.IsNotExist(err) {
		http.Error(w, "yaml file does not exist", http.StatusBadRequest)
		log.Println("yaml file does not exist")
		return
	}

	loggerCmd = exec.Command("./logger", "-c", yamlPath)
	if err := loggerCmd.Start(); err != nil {
		http.Error(w, "Failed to start logger", http.StatusInternalServerError)
		log.Println("Failes to start logger")
		loggerCmd = nil
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(Response{Message: "Logger started successfully"})
	log.Println("Logger started successfully")
}

func stopLogger(w http.ResponseWriter, r *http.Request) {
	mutex.Lock()
	defer mutex.Unlock()

	if loggerCmd == nil || loggerCmd.Process == nil {
		http.Error(w, "Logger is not running", http.StatusBadRequest)
		log.Println("Logger is not running")
		return
	}

	if err := loggerCmd.Process.Kill(); err != nil {
		http.Error(w, "Failed to stop logger", http.StatusInternalServerError)
		log.Println("Failed to stop logger")
		return
	}

	loggerCmd = nil
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(Response{Message: "Logger stopped successfully"})
	log.Println("Logger stopped successfully")
}

func getLogs(w http.ResponseWriter, r *http.Request) {
	logFilePath := filepath.Join(uploadDir, "logger.log")
	logs, err := ioutil.ReadFile(logFilePath)
	if err != nil {
		http.Error(w, "Failed to read logs", http.StatusInternalServerError)
		log.Println("Failed to read logs")
		return
	}

	w.Header().Set("Content-Type", "text/plain")
	w.Write(logs)
	log.Println("Reading logs successfully")
}

func getAssignments(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	data, err := os.ReadFile(assignmentsFile)
	if err != nil {
		if os.IsNotExist(err) {
			data = []byte(`[{"CANSocket": "can0", "DBCFile": "", "YAMLFile": ""}]`)
			log.Println("Assignments file does not exist. Creating assignments file with standard assignments")
			if err := os.WriteFile(assignmentsFile, data, 0644); err != nil {
				http.Error(w, "Failed to create assignments file", http.StatusInternalServerError)
				log.Println("Failed to create assignments file:", err)
				return
			}
		} else {
			http.Error(w, "Failed to read assignments", http.StatusInternalServerError)
			log.Println("Failed to read assignments:", err)
			return
		}
	} else if len(data) == 2 {
		log.Println("Assignments file empty. Creating standard assignments")
		data = []byte(`[{"CANSocket": "can0", "DBCFile": "", "YAMLFile": ""}]`)
		if err := os.WriteFile(assignmentsFile, data, 0644); err != nil {
			http.Error(w, "Failed to write default assignments", http.StatusInternalServerError)
			log.Println("Failed to write default assignments:", err)
			return
		}
	}

	w.Write(data)
}

func saveAssignments(w http.ResponseWriter, r *http.Request) {
    w.Header().Set("Content-Type", "application/json")

    var newAssignments []CANAssignment
    if err := json.NewDecoder(r.Body).Decode(&newAssignments); err != nil {
        http.Error(w, "Invalid JSON", http.StatusBadRequest)
        log.Println("Invalid JSON file.")
        return
    }

    // Reading assignments from file
    data, err := os.ReadFile(assignmentsFile)
    if err != nil {
        http.Error(w, "Failed to read current assignments", http.StatusInternalServerError)
        log.Println("Failed to read current assignments:", err)
        return
    }

    // changing current assignment into slice of CANAssignment
    var currentAssignments []CANAssignment
    if len(data) > 0 {
        if err := json.Unmarshal(data, &currentAssignments); err != nil {
            http.Error(w, "Failed to unmarshal current assignments", http.StatusInternalServerError)
            log.Println("Failed to unmarshal current assignments:", err)
            return
        }
    }

    // add or overwrite current assignment
    for i, newAssign := range newAssignments {
        if i < len(currentAssignments) {
            // overwrite
            currentAssignments[i].CANSocket = newAssign.CANSocket
            currentAssignments[i].DBCFile = newAssign.DBCFile
            currentAssignments[i].YAMLFile = newAssign.YAMLFile
        } else {
            // add
            currentAssignments = append(currentAssignments, newAssign)
        }
    }

	// serialize current assignment and parsing into JSON
    dataToSave, err := json.MarshalIndent(currentAssignments, "", "  ")
    if err != nil {
        http.Error(w, "Failed to serialize assignments", http.StatusInternalServerError)
        log.Println("Failed to serialize assignments:", err)
        return
    }

    if err := os.WriteFile(assignmentsFile, dataToSave, 0644); err != nil {
        http.Error(w, "Failed to save assignments", http.StatusInternalServerError)
        log.Println("Failed to save assignments:", err)
        return
    }

    // successful answer
    w.WriteHeader(http.StatusOK)
    json.NewEncoder(w).Encode(Response{Message: "Assignments saved successfully"})
    log.Println("Assignment saved successfully.")
}

func deleteAssignment(w http.ResponseWriter, r *http.Request) {
    w.Header().Set("Content-Type", "application/json")

    // extracting index from URL
    path := r.URL.Path
    parts := strings.Split(path, "/")
    if len(parts) < 3 {
        http.Error(w, "Invalid URL", http.StatusBadRequest)
        log.Println("Invalid URL:", path)
        return
    }

    index, err := strconv.Atoi(parts[2]) 
    if err != nil {
        http.Error(w, "Invalid index", http.StatusBadRequest)
        log.Println("Invalid Index:", err)
        return
    }

    // Read assignment from file
    data, err := os.ReadFile(assignmentsFile)
    if err != nil {
        http.Error(w, "Failed to read assignments", http.StatusInternalServerError)
        log.Println("Failed to read assignments:", err)
        return
    }

    // parsing assignments
    var assignments []CANAssignment
    if len(data) > 0 {
        if err := json.Unmarshal(data, &assignments); err != nil {
            http.Error(w, "Failed to unmarshal assignments", http.StatusInternalServerError)
            log.Println("Failed to unmarshal assignments:", err)
            return
        }
    }

    if index < 0 || index >= len(assignments) {
        http.Error(w, "Index out of range", http.StatusBadRequest)
        log.Println("Index out of range:", index)
        return
    }

    // deleting assignment
    assignments = append(assignments[:index], assignments[index+1:]...)

    // save current assignment
    dataToSave, err := json.MarshalIndent(assignments, "", "  ")
    if err != nil {
        http.Error(w, "Failed to serialize assignments", http.StatusInternalServerError)
        log.Println("Failed to serialize assignments:", err)
        return
    }

    if err := os.WriteFile(assignmentsFile, dataToSave, 0644); err != nil {
        http.Error(w, "Failed to save assignments", http.StatusInternalServerError)
        log.Println("Failed to save assignments:", err)
        return
    }

	// successful answer
    w.WriteHeader(http.StatusOK)
    json.NewEncoder(w).Encode(Response{Message: "Assignment deleted successfully"})
    log.Println("Assignment deleted successfully")
}

func uploadHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusOK)
		return
	}

	if r.Method != http.MethodPost {
		log.Println("Invalid request method:", r.Method)
		http.Error(w, "Invalid request method", http.StatusMethodNotAllowed)
		return
	}

	file, handler, err := r.FormFile("file")
	if err != nil {
		log.Println("Failed to get file from form:", err)
		http.Error(w, "Failed to get file from form", http.StatusBadRequest)
		return
	}
	defer file.Close()

	filePath := filepath.Join(uploadDir, handler.Filename)
	saveFile, err := os.Create(filePath)
	if err != nil {
		log.Println("Failed to save file:", err)
		http.Error(w, "Failed to save file", http.StatusInternalServerError)
		return
	}
	defer saveFile.Close()

	if _, err := io.Copy(saveFile, file); err != nil {
		http.Error(w, "Failed to save file", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("File uploaded successfully:" + handler.Filename))
	log.Println("File uploaded successfully:", handler.Filename)
}

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


func main () {
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

	// create upload directory
	if _, err := os.Stat(uploadDir); os.IsNotExist(err) {
		if err := os.MkdirAll(uploadDir, os.ModePerm); err != nil {
			log.Panicf("Failed to create upload directory: %v", err)
		}
		log.Println("Creating upload directory successfully:", uploadDir)
	} else {
		log.Println("Upload directory already exists:", uploadDir)
	}


	mux := http.NewServeMux()

	// Logger Endpoints
	mux.Handle("/logger/start", corsMiddleware(http.HandlerFunc(startLogger)))
	mux.Handle("/logger/stop", corsMiddleware(http.HandlerFunc(stopLogger)))
	mux.Handle("/logger/logs", corsMiddleware(http.HandlerFunc(getLogs)))
	mux.Handle("/logger/files", corsMiddleware(http.HandlerFunc(listFiles)))

	// Upload Endpoint
	mux.Handle("/upload", corsMiddleware(http.HandlerFunc(uploadHandler)))

	// Assignments Endpoints
	mux.HandleFunc("/assignments", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			getAssignments(w, r)
		} else if r.Method == http.MethodPost {
			saveAssignments(w, r)
		} else {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	})
	mux.Handle("/assignments/", corsMiddleware(http.HandlerFunc(deleteAssignment)))

	// WebSocket Endpoint
	mux.HandleFunc("/ws", handleWebSocket)

	// Static File Server
	mux.Handle("/", http.FileServer(http.Dir("./static")))

	serverAddr := fmt.Sprintf(":%s", *port)
	log.Printf("Server läuft auf http://localhost%s", serverAddr)
	log.Fatal(http.ListenAndServe(serverAddr, mux))

}