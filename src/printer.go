package main

import (
	"fmt"
	"time"

	"tinygo.org/x/bluetooth"
)

const (
	// BLE Service and Characteristics UUIDs
	MainServiceUUID = "0000ae30-0000-1000-8000-00805f9b34fb"
	ControlCharUUID = "0000ae01-0000-1000-8000-00805f9b34fb" // AE01 - Control
	NotifyCharUUID  = "0000ae02-0000-1000-8000-00805f9b34fb" // AE02 - Notify
	DataCharUUID    = "0000ae03-0000-1000-8000-00805f9b34fb" // AE03 - Data

	// Protocol constants
	Preamble1 = 0x22
	Preamble2 = 0x21
	Footer    = 0xFF

	// Command IDs
	CmdGetStatus     = 0xA1
	CmdSetIntensity  = 0xA2
	CmdPrintRequest  = 0xA9
	CmdFlushData     = 0xAD
	CmdPrintComplete = 0xAA
	CmdGetBattery    = 0xAB
	CmdGetVersion    = 0xB1
	CmdCancelPrint   = 0xAC

	// Image constants
	ImageWidth      = 384  // pixels
	ImageWidthBytes = 48   // 384/8
	MinImageBytes   = 4320 // minimum padding
)

type CatPrinter struct {
	adapter       *bluetooth.Adapter
	device        bluetooth.Device
	controlChar   bluetooth.DeviceCharacteristic
	notifyChar    bluetooth.DeviceCharacteristic
	dataChar      bluetooth.DeviceCharacteristic
	connected     bool
	notifications chan []byte
	lastStatus    *PrinterStatus
}

type PrinterStatus struct {
	Connected    bool
	Battery      int
	Temperature  int
	Status       int    // 0=Standby, 1=Printing
	ErrorFlag    int    // 0=OK, anything else is an error
	ErrorCode    int    // Error details
	StatusString string // Human-readable status
}

func ifErrNotNil(err error, message string) {
	if err != nil {
		fmt.Printf("Error: %s: %v\n", message, err)
	}
}

func NewCatPrinter() (*CatPrinter, error) {
	adapter := bluetooth.DefaultAdapter
	err := adapter.Enable()
	ifErrNotNil(err, "failed to enable Bluetooth adapter")

	printer := &CatPrinter{
		adapter:       adapter,
		notifications: make(chan []byte, 10),
		lastStatus: &PrinterStatus{
			StatusString: "Not connected",
		},
	}
	return printer, nil
}

func (p *CatPrinter) Connect() error {
	if p.connected {
		return nil
	}

	fmt.Println("Scanning for cat printer...")
	var printerAddr bluetooth.Address
	found := false

	// CONNECT TO OUR FUCKING PRINTER
	knownName := "MXW01"

	err := p.adapter.Scan(func(adapter *bluetooth.Adapter, device bluetooth.ScanResult) {
		name := device.LocalName()
		fmt.Printf("Found device: %s (%s)\n", name, device.Address.String())

		if name == knownName {
			printerAddr = device.Address
			found = true
			adapter.StopScan()
			return
		}
	})

	ifErrNotNil(err, "failed to start scan")

	// Wait for device to be found
	timeout := time.After(10 * time.Second)
	for !found {
		select {
		case <-timeout:
			return fmt.Errorf("printer not found within timeout")
		default:
			time.Sleep(100 * time.Millisecond)
		}
	}

	fmt.Printf("Connecting to printer at %s...\n", printerAddr.String())

	// Connect to device
	device, err := p.adapter.Connect(printerAddr, bluetooth.ConnectionParams{})
	ifErrNotNil(err, "failed to connect to printer")

	p.device = *device

	// Discover services
	services, err := device.DiscoverServices(nil)
	ifErrNotNil(err, "failed to discover services")

	// Find the main service
	var mainService bluetooth.DeviceService
	for _, svc := range services {
		if svc.UUID().String() == MainServiceUUID {
			mainService = svc
			break
		}
	}

	if mainService.UUID().String() == "" {
		return fmt.Errorf("main service not found")
	}

	// Discover characteristics
	chars, err := mainService.DiscoverCharacteristics(nil)
	ifErrNotNil(err, "failed to discover characteristics")

	// Map characteristics
	for _, char := range chars {
		switch char.UUID().String() {
		case ControlCharUUID:
			p.controlChar = char
		case NotifyCharUUID:
			p.notifyChar = char
		case DataCharUUID:
			p.dataChar = char
		}
	}

	// Verify all characteristics found
	if p.controlChar.UUID().String() == "" || p.notifyChar.UUID().String() == "" || p.dataChar.UUID().String() == "" {
		return fmt.Errorf("not all required characteristics found")
	}

	// Enable notifications with debug logging
	err = p.notifyChar.EnableNotifications(func(buf []byte) {
		fmt.Printf("DEBUG: Received notification with %d bytes: %x\n", len(buf), buf)
		select {
		case p.notifications <- buf:
			fmt.Println("DEBUG: Notification sent to channel")
		default:
			fmt.Println("DEBUG: Channel full, dropping notification")
		}
	})
	if err != nil {
		return fmt.Errorf("failed to enable notifications: %v", err)
	}

	fmt.Println("DEBUG: Notifications enabled successfully")

	p.connected = true
	fmt.Println("Successfully connected to cat printer!")

	// Update status
	p.UpdateStatus()

	return nil
}

func (p *CatPrinter) Disconnect() error {
	if !p.connected {
		return nil
	}

	err := p.device.Disconnect()
	p.connected = false
	p.lastStatus.Connected = false
	p.lastStatus.StatusString = "Disconnected"

	return err
}

// Add CRC calculation function
func calculateCRC8(data []byte) byte {
	// CRC-8 / DALLAS-MAXIM
	// Polynomial: 0x07 (x^8 + x^2 + x^1 + x^0)
	// Initial Value: 0x00
	// Reflect Input: False
	// Reflect Output: False
	// XOR Output: 0x00
	crc := byte(0x00)
	for _, b := range data {
		crc ^= b
		for i := 0; i < 8; i++ {
			if crc&0x80 != 0 {
				crc = (crc << 1) ^ 0x07
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

func (p *CatPrinter) UpdateStatus() error {
	if !p.connected {
		return fmt.Errorf("printer not connected")
	}

	fmt.Println("Updating printer status...")

	// Clear any pending notifications before sending command
	select {
	case <-p.notifications:
		fmt.Println("DEBUG: Cleared pending notification")
	default:
		// No pending notifications, continue
	}

	// Build proper control packet for Get Status (A1) command
	payload := []byte{0x00}
	crc := calculateCRC8(payload)

	packet := []byte{
		Preamble1,    // 0x22
		Preamble2,    // 0x21
		CmdGetStatus, // 0xA1
		0x00,         // Fixed byte
		0x01, 0x00,   // Length (1 byte payload, little endian)
		0x00,   // Payload
		crc,    // CRC8 checksum
		Footer, // 0xFF
	}

	// Send command to get status via control characteristic
	_, err := p.controlChar.WriteWithoutResponse(packet)
	if err != nil {
		return fmt.Errorf("failed to send status command: %v", err)
	}

	// Wait for response
	select {
	case buf := <-p.notifications:
		return p.parseStatusResponse(buf)
	case <-time.After(5 * time.Second):
		return fmt.Errorf("status response timeout")
	}
}

func (p *CatPrinter) parseStatusResponse(buf []byte) error {

	if len(buf) < 9 {
		return fmt.Errorf("invalid status response length: %d", len(buf))
	}

	// Verify preamble
	if buf[0] != Preamble1 || buf[1] != Preamble2 {
		return fmt.Errorf("invalid response preamble: got %02x%02x, expected %02x%02x",
			buf[0], buf[1], Preamble1, Preamble2)
	}

	// Verify command ID
	if buf[2] != CmdGetStatus {
		return fmt.Errorf("unexpected response command ID: 0x%02X", buf[2])
	}

	// Parse length (bytes 4-5, little endian)
	payloadLength := int(buf[4]) | (int(buf[5]) << 8)

	if len(buf) < 6+payloadLength {
		return fmt.Errorf("response too short for declared payload length: got %d, need %d",
			len(buf), 6+payloadLength)
	}

	fmt.Printf("DEBUG: Status response raw: %x (length: %d)\n", buf, len(buf))

	// Based on protocol documentation and observed responses
	if len(buf) >= 13 { // Need at least 13 bytes to access payload[12]
		p.lastStatus.Connected = true

		// Safe access with bounds checking
		if len(buf) > 9 {
			p.lastStatus.Battery = int(buf[9])
		}
		if len(buf) > 10 {
			p.lastStatus.Temperature = int(buf[10])
		}
		if len(buf) > 6 {
			p.lastStatus.Status = int(buf[6]) // 0x00 = Standby, 0x01 = Printing, etc
		}

		// Check error flag
		if len(buf) > 12 {
			p.lastStatus.ErrorFlag = int(buf[12]) // 0x00 = OK, anything else is an error
			if p.lastStatus.ErrorFlag != 0 && len(buf) > 13 {
				p.lastStatus.ErrorCode = int(buf[13]) // Error details
			}
		}

		p.lastStatus.StatusString = fmt.Sprintf("Battery: %d%%, Temp: %d°C, Status: %d, Error: %d",
			p.lastStatus.Battery, p.lastStatus.Temperature, p.lastStatus.Status, p.lastStatus.ErrorFlag)

	} else {
		return fmt.Errorf("status payload too short: %d bytes", len(buf))
	}

	fmt.Printf("Printer Status: Connected=%v, Battery=%d%%, Temp=%d°C, Status=%s\n",
		p.lastStatus.Connected, p.lastStatus.Battery, p.lastStatus.Temperature, p.lastStatus.StatusString)

	return nil
}

func (p *CatPrinter) TestPrintCard() error {
	// Create proper test image data for 10 lines of 384 pixels each
	// Each line needs 48 bytes (384 pixels / 8 bits per byte)
	lineBytes := ImageWidthBytes // 48 bytes per line
	lineCount := 10
	totalBytes := lineBytes * lineCount // 480 bytes

	imageData := make([]byte, totalBytes)

	// Create a simple test pattern: alternating black and white blocks
	for line := 0; line < lineCount; line++ {
		for byteIdx := 0; byteIdx < lineBytes; byteIdx++ {
			// Create alternating pattern every 8 pixels (1 byte)
			if (byteIdx % 2) == (line % 2) {
				imageData[line*lineBytes+byteIdx] = 0xFF // Black pixels
			} else {
				imageData[line*lineBytes+byteIdx] = 0x00 // White pixels
			}
		}
	}

	fmt.Printf("Created test image: %d lines, %d bytes total\n", lineCount, len(imageData))

	p.UpdateStatus()
	p.setIntensity(93) // Set intensity to 93 (0x5D)

	fmt.Println("DEBUG: Starting print request...")

	// Clear any pending notifications
	select {
	case <-p.notifications:
		fmt.Println("DEBUG: Cleared pending notification before print request")
	default:
	}

	// Check for errors
	if p.lastStatus.ErrorFlag != 0 {
		return fmt.Errorf("printer error: %d", p.lastStatus.ErrorFlag)
	}

	// Check if not currently printing
	if p.lastStatus.Status != 0 {
		return fmt.Errorf("printer is currently printing, cannot start new print")
	}

	// Build A9 Print request payload
	// Convert line count to little endian (2 bytes)
	lineCountLE := []byte{
		byte(lineCount & 0xFF),        // Low byte
		byte((lineCount >> 8) & 0xFF), // High byte
	}

	payload := []byte{
		lineCountLE[0], // Low byte of line count
		lineCountLE[1], // High byte of line count
		0x30,           // Fixed byte
		0x00,           // Mode: 0x00 = 1bpp
	}

	crc := calculateCRC8(payload)

	// Build complete packet with correct structure
	packet := []byte{
		Preamble1,       // 0x22
		Preamble2,       // 0x21
		CmdPrintRequest, // 0xA9
		0x00,            // Fixed byte
		0x04, 0x00,      // Length (4 bytes payload, little endian)
		lineCountLE[0], // Payload: line count low byte
		lineCountLE[1], // Payload: line count high byte
		0x30,           // Payload: fixed byte
		0x00,           // Payload: mode (1bpp)
		crc,            // CRC8 checksum
		Footer,         // 0xFF
	}

	// Send print request command
	_, err := p.controlChar.WriteWithoutResponse(packet)
	if err != nil {
		return fmt.Errorf("failed to send print request command: %v", err)
	}

	fmt.Println("Sent Print request command, waiting for response...")

	// Wait for A9 response
	select {
	case buf := <-p.notifications:
		fmt.Printf("DEBUG: Print request response: %x\n", buf)

		// Verify response structure and check acceptance
		if len(buf) >= 7 {
			// Check if response is for A9 command
			if buf[2] == CmdPrintRequest {
				// Check response payload (should be 0x00 for acceptance)
				payloadStart := 6 // After preamble, cmd, unknown, length
				if buf[payloadStart] == 0x00 {
					fmt.Println("Print request accepted!")
					return p.transferImageData(imageData)
				} else {
					return fmt.Errorf("print request rejected, response code: 0x%02X", buf[payloadStart])
				}
			} else {
				return fmt.Errorf("unexpected response command: 0x%02X", buf[2])
			}
		} else {
			return fmt.Errorf("response too short: %d bytes", len(buf))
		}
	case <-time.After(5 * time.Second):
		return fmt.Errorf("print request timeout")
	}
}

func (p *CatPrinter) transferImageData(imageData []byte) error {
	fmt.Println("DEBUG: Starting image data transfer...")

	// Ensure minimum padding (4320 bytes minimum according to protocol) (about 90 lines)
	paddedData := imageData
	if len(paddedData) < MinImageBytes {
		padding := make([]byte, MinImageBytes-len(paddedData))
		paddedData = append(paddedData, padding...)
		fmt.Printf("Padded image data to %d bytes\n", len(paddedData))
	}

	// send data in chunks of to avoid buffer overflow
	chunkSize := 20 // 20 bytes per chunk

	for i := 0; i < len(paddedData); i += chunkSize {
		end := i + chunkSize
		if end > len(paddedData) {
			end = len(paddedData)
		}
		chunk := paddedData[i:end]

		fmt.Printf("Sending chunk %d: %x\n", i/chunkSize, chunk)
		_, err := p.dataChar.WriteWithoutResponse(chunk)
		ifErrNotNil(err, fmt.Sprintf("failed to send data chunk %d", i/chunkSize))

		time.Sleep(10 * time.Millisecond) // small delay to avoid flooding the buffer

		if (i/chunkSize)%10 == 0 { // Progress update every 10 chunks
			fmt.Printf("Sent %d/%d bytes (%.1f%%)\n",
				end, len(paddedData),
				float64(end)/float64(len(paddedData))*100)
		}
	}
	fmt.Println("Image data transfer complete, sending flush command...")
	err := p.flushData()
	ifErrNotNil(err, "failed to flush data after transfer")

	return nil

}

func (p *CatPrinter) flushData() error {
	if !p.connected {
		return fmt.Errorf("printer not connected")
	}

	// Build AD (Flush Data) command packet
	payload := []byte{0x00}
	crc := calculateCRC8(payload)

	packet := []byte{
		Preamble1,    // 0x22
		Preamble2,    // 0x21
		CmdFlushData, // 0xAD
		0x00,         // Fixed byte
		0x01, 0x00,   // Length (1 byte payload, little endian)
		0x00,   // Payload
		crc,    // CRC8 checksum
		Footer, // 0xFF
	}

	// Send flush command via Control Characteristic (AE01)
	_, err := p.controlChar.WriteWithoutResponse(packet)
	if err != nil {
		return fmt.Errorf("failed to send flush command: %v", err)
	}

	fmt.Println("Flush command sent, waiting for print completion...")

	// Wait for AA (Print Complete) notification
	select {
	case buf := <-p.notifications:
		fmt.Printf("DEBUG: Print completion response: %x\n", buf)
		if len(buf) >= 3 && buf[2] == CmdPrintComplete {
			fmt.Println("Print completed successfully!")
			return nil
		} else {
			return fmt.Errorf("unexpected response after flush: %x", buf)
		}
	case <-time.After(30 * time.Second): // Longer timeout for printing
		return fmt.Errorf("print completion timeout")
	}
}

func (p *CatPrinter) setIntensity(intensity int) error {
	if !p.connected {
		return fmt.Errorf("printer not connected")
	}

	// Validate intensity range (0x00 to 0xFF)
	if intensity < 0 || intensity > 255 {
		return fmt.Errorf("intensity must be between 0 and 255, got %d", intensity)
	}

	fmt.Printf("Setting print intensity to %d (0x%02X)...\n", intensity, intensity)

	// Build payload with intensity value
	payload := []byte{byte(intensity)}
	crc := calculateCRC8(payload)

	// Build control packet for Set Intensity (A2) command
	packet := []byte{
		Preamble1,       // 0x22
		Preamble2,       // 0x21
		CmdSetIntensity, // 0xA2
		0x00,            // Fixed byte
		0x01, 0x00,      // Length (1 byte payload, little endian)
		byte(intensity), // Payload: intensity value
		crc,             // CRC8 checksum
		Footer,          // 0xFF
	}

	// Send command via control characteristic
	_, err := p.controlChar.WriteWithoutResponse(packet)
	if err != nil {
		return fmt.Errorf("failed to send set intensity command: %v", err)
	}

	fmt.Printf("Intensity set to %d successfully\n", intensity)
	return nil
}
