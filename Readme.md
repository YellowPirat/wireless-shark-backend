# wireless-shark-backend

A Go-based application that bridges CAN bus interfaces to WebSocket clients, enabling real-time monitoring of CAN frames through a web interface.

## Features

- Multiple CAN interface support (e.g., vcan0, vcan1)
- Real-time CAN frame broadcasting via WebSocket
- JSON-formatted frame data
- Debug output option
- Clean shutdown handling

## Getting Started

### Prerequisites

- Linux operating system
- Go 1.13 or higher
- CAN bus interface(s) configured on the system

### Installation

#### Precompiled releases

There are precompiled releases for amd64 and arm64 under the release page avaiable.
https://github.com/YellowPirat/wireless-shark-backend/releases

#### Build from source

1. Clone the repository:
```bash
git clone git@github.com:YellowPirat/wireless-shark-backend.git
cd wireless-shark-backend
```

2. Build the application:
```bash
go build
```

## Usage

Start the application by specifying CAN interfaces:

```bash
./main -interfaces vcan0,vcan1 -port 8080 -debug true
```

### Command Line Arguments

- `-interfaces`: Comma-separated list of CAN interfaces (required)
- `-port`: WebSocket server port (default: "8080")
- `-debug`: Enable debug output (default: false)

## WebSocket Protocol

The server broadcasts CAN frames as JSON messages with the following format:

```json
{
    "id": 123,              // CAN frame ID
    "length": 8,            // Data length
    "data": [1,2,3,4,5,6,7,8], // Frame data (up to 8 bytes)
    "timestamp": "2025-01-27T12:34:56.789Z", // Frame reception time
    "socket_id": "vcan0"    // Source interface
}
```

## Example Client Connection

```javascript
const ws = new WebSocket('ws://localhost:8080/ws');

ws.onmessage = function(event) {
    const frame = JSON.parse(event.data);
    console.log('Received CAN frame:', frame);
};
```

## Development Setup

For testing, you can set up virtual CAN interfaces:

```bash
# Load vcan kernel module
sudo modprobe vcan

# Create a virtual CAN interface
sudo ip link add dev vcan0 type vcan
sudo ip link set up vcan0
```

## Error Handling

The application includes robust error handling for:
- CAN socket creation and binding
- WebSocket connections
- Frame reception and broadcasting

Error messages are logged with appropriate context for debugging.

## Performance Considerations

- The application uses blocking reads for CAN frames
- Each CAN interface runs in its own goroutine
- WebSocket broadcasts are handled concurrently
- No artificial delays in the read loop

## License
Wireless Shark is distributed under the terms of the GPL-3.0 license.

## Acknowledgments
Created by TI 5 in WiSe 2024/25

Ahmet Emirhan Göktaş
Benjamin Klaric
Jannis Gröger
Mario Wegman
Maximilian Hoffmann
Stasa Lukic
