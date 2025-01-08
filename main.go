package main

import (
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"log"
	"sync"
	"time"
	"github.com/gorilla/websocket"
)

// Konstante für das Upload-Verzeichnis und die Zuweisungen-Datei
const (
	uploadDir       = "/var/www"
	assignmentsFile = "/var/www/can_assignments.json"
)

var (
	loggerCmd *exec.Cmd
	mutex     sync.Mutex
)

// Strukturen
type Response struct {
	Message string `json:"message"`
}

type CANFrame struct {
	ID        uint32    `json:"id"`
	Length    uint8     `json:"length"`
	Data      [8]byte   `json:"data"`
	Timestamp time.Time `json:"timestamp"`
}

type CANAssignment struct {
	CANSocket string `json:"can_socket"`
	DBCFile   string `json:"dbc_file"`
	YAMLFile  string `json:"yaml_file"`
}


//Middleware zum Setzen der CORS Header 
func corsMiddleware(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        // CORS-Header setzen
        w.Header().Set("Access-Control-Allow-Origin", "*")  // Erlaube Anfragen von http://localhost:3000
        w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")    // Erlaube GET, POST und OPTIONS
        w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")  // Erlaube bestimmte Header
        w.Header().Set("Access-Control-Allow-Credentials", "true")  // Erlaube Cookies/Anmeldeinformationen

        // Wenn es eine OPTIONS-Anfrage ist, direkt mit 200 OK antworten
        if r.Method == "OPTIONS" {
            w.WriteHeader(http.StatusOK)
            return
        }

        // Weiter mit der Anfrage
        next.ServeHTTP(w, r)
    })
}

func listConfigFiles(w http.ResponseWriter, r *http.Request) {
    files, err := os.ReadDir("/var/www")
    if err != nil {
        http.Error(w, "Failed to read config files", http.StatusInternalServerError)
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
}

// Logger starten
func startLogger(w http.ResponseWriter, r *http.Request) {
	mutex.Lock()
	defer mutex.Unlock()

	if loggerCmd != nil && loggerCmd.Process != nil {
		http.Error(w, "Logger is already running", http.StatusConflict)
		fmt.Println("Logger läuft bereits.")
		return
	}

	configFile := r.URL.Query().Get("config")
	if configFile == "" {
		http.Error(w, "Missing config file parameter", http.StatusBadRequest)
		fmt.Println("Config File Parameter fehlt.")
		return
	}

	configPath := filepath.Join(uploadDir, configFile)
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		http.Error(w, "Config file does not exist", http.StatusBadRequest)
		fmt.Println("Config File existiert nicht.")
		return
	}

	loggerCmd = exec.Command("./logger", "-c", configPath)
	if err := loggerCmd.Start(); err != nil {
		http.Error(w, "Failed to start logger", http.StatusInternalServerError)
		fmt.Println("Starten des Loggers fehlgeschlagen.")
		loggerCmd = nil
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(Response{Message: "Logger started successfully"})
	fmt.Println("Logger erfolgreich gestartet.")
}

// Logger stoppen
func stopLogger(w http.ResponseWriter, r *http.Request) {
	mutex.Lock()
	defer mutex.Unlock()

	if loggerCmd == nil || loggerCmd.Process == nil {
		http.Error(w, "Logger is not running", http.StatusBadRequest)
		fmt.Println("Logger läuft nicht.")
		return
	}

	if err := loggerCmd.Process.Kill(); err != nil {
		http.Error(w, "Failed to stop logger", http.StatusInternalServerError)
		fmt.Println("Stoppen des Loggers fehlgeschlagen.")
		return
	}

	loggerCmd = nil
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(Response{Message: "Logger stopped successfully"})
	fmt.Println("Logger erfolgreich gestoppt.")
}

// Logs abrufen
func getLogs(w http.ResponseWriter, r *http.Request) {
	logFilePath := filepath.Join(uploadDir, "logger.log")
	logs, err := ioutil.ReadFile(logFilePath)
	if err != nil {
		http.Error(w, "Failed to read logs", http.StatusInternalServerError)
		fmt.Println("Lesen der Logs fehlgeschlagen.")
		return
	}

	w.Header().Set("Content-Type", "text/plain")
	w.Write(logs)
	fmt.Println("Logs erfolgreich ausgelesen.")
}

// Bestehende CAN-Zuweisungen abrufen
func getAssignments(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	data, err := os.ReadFile(assignmentsFile)
	if err != nil {
		if os.IsNotExist(err) {
			data = []byte("[]")
		} else {
			http.Error(w, "Failed to read assignments", http.StatusInternalServerError)
			return
		}
	}

	w.Write(data)
}

// Neue CAN-Zuweisungen speichern
func saveAssignments(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	var assignments []CANAssignment
	if err := json.NewDecoder(r.Body).Decode(&assignments); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	data, err := json.MarshalIndent(assignments, "", "  ")
	if err != nil {
		http.Error(w, "Failed to serialize assignments", http.StatusInternalServerError)
		return
	}

	if err := os.WriteFile(assignmentsFile, data, 0644); err != nil {
		http.Error(w, "Failed to save assignments", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(Response{Message: "Assignments saved successfully"})
	fmt.Println("Zuweisungen erfolgreich gespeichert.")
}

// Datei-Upload
func uploadHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusOK)
		return
	}

	if r.Method != http.MethodPost {
		fmt.Println("Ungültige Request Methode:", r.Method)
		http.Error(w, "Invalid request method", http.StatusMethodNotAllowed)
		return
	}

	file, handler, err := r.FormFile("file")
	if err != nil {
		fmt.Println("Datei hochladen fehlgeschlagen:", err)
		http.Error(w, "Failed to get file from form", http.StatusBadRequest)
		return
	}
	defer file.Close()

	filePath := filepath.Join(uploadDir, handler.Filename)
	saveFile, err := os.Create(filePath)
	if err != nil {
		fmt.Println("Datei speichern fehlgeschlagen:", err)
		http.Error(w, "Failed to save file", http.StatusInternalServerError)
		return
	}
	defer saveFile.Close()

	if _, err := io.Copy(saveFile, file); err != nil {
		http.Error(w, "Failed to save file", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "File uploaded successfully: %s", handler.Filename)
}

// WebSocket (optional)
func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*") 
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket Upgrade Fehler: %v", err)
		return
	}
	defer conn.Close()

	for {
		_, _, err := conn.ReadMessage()
		if err != nil {
			log.Printf("Fehler beim Lesen der WebSocket-Nachricht: %v", err)
			break
		}
	}
}

func main() {
	//erstellt einen neuen Router/Multiplexer
	mux := http.NewServeMux()

	// Initialisierung
	mux.Handle("/logger/start",corsMiddleware(http.HandlerFunc(startLogger)))
	mux.Handle("/logger/stop",corsMiddleware(http.HandlerFunc(stopLogger)))
	mux.Handle("/logger/logs",corsMiddleware(http.HandlerFunc(getLogs)))
	mux.Handle("/logger/configs",corsMiddleware(http.HandlerFunc(listConfigFiles)))
	mux.Handle("/upload",corsMiddleware(http.HandlerFunc(uploadHandler)))
	mux.Handle("/assignments", corsMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			getAssignments(w, r)
		} else if r.Method == http.MethodPost {
			saveAssignments(w, r)
		} else {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		}
	})))

	mux.HandleFunc("/ws", handleWebSocket)

	// Upload-Verzeichnis erstellen
	if _, err := os.Stat(uploadDir); os.IsNotExist(err) {
		if err := os.MkdirAll(uploadDir, os.ModePerm); err != nil {
			panic(fmt.Sprintf("Failed to create upload directory: %v", err))
		}
	}

	fmt.Println("Server läuft auf http://localhost:8080")
	log.Fatal(http.ListenAndServe(":8080", mux))
}
