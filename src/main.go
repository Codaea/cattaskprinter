package main

import (
	"fmt"
	"log"
	"net/http"

	"github.com/gin-gonic/gin"
)

type PrintRequest struct {
	Text     string `json:"text"`
	QRCode   string `json:"qr_code,omitempty"`
	FontSize int    `json:"font_size,omitempty"`
}

type PrintResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

type StatusResponse struct {
	Connected bool   `json:"connected"`
	Battery   int    `json:"battery"`
	Status    string `json:"status"`
	Error     string `json:"error,omitempty"`
}

var p *CatPrinter

func main() {
	// Initialize the cat printer
	var err error
	p, err = NewCatPrinter()
	if err != nil {
		log.Fatalf("Failed to initialize printer: %v", err)
	}

	// Setup HTTP server
	r := gin.Default()

	// Enable CORS
	r.Use(func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Content-Type")
		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(204)
			return
		}
		c.Next()
	})

	// API endpoints
	r.GET("/status", getStatus)
	r.GET("/test-print", testPrint)
	r.POST("/print")

	// Start server
	fmt.Println("Cat Printer Server starting on port 8080...")
	fmt.Println("API Endpoints:")
	fmt.Println("  GET  /status       - Get printer status")

	// connect to printer after starting webserver
	p.Connect()

	log.Fatal(r.Run(":8080"))
}

func getStatus(c *gin.Context) {
	p.UpdateStatus()
	status := StatusResponse{
		Connected: p.lastStatus.Connected,
		Battery:   p.lastStatus.Battery,
		Status:    p.lastStatus.StatusString,
	}
	c.JSON(http.StatusOK, status)
}

func testPrint(c *gin.Context) {
	p.TestPrintCard()
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "Test print successful",
	})
}
