package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/ukarim/smscsim/smsc"
	"github.com/ukarim/smscsim/smscserver"
)

func main() {
	smscPort := getPort("SMSC_PORT", 2775)
	webPort := getPort("WEB_PORT", 12775)

	// start smpp server
	service := smsc.NewSmsc()
	messageChan := make(smsc.MessageChan)
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go service.Start(smscPort, messageChan)

	// start web server
	webServer := smscserver.NewWebServer(service)
	go webServer.Start(webPort)

	go func() {
	forloop:
		for {
			select {
			case sig := <-sigChan:
				fmt.Printf("Received signal: %v\n", sig)
				break forloop
			case message := <-messageChan:
				fmt.Println("received message", message.MessageReceived(), message.MessageId(), message.Command(), message.Response(), message.Command())
			}
		}
	}()
	<-sigChan
}

func getPort(envVar string, defVal int) int {
	port := defVal
	portStr := os.Getenv(envVar)
	if portStr != "" {
		p, err := strconv.Atoi(portStr)
		if err != nil || p < 1 {
			log.Fatalf("invalid port %s [%s]", envVar, portStr)
		} else {
			port = p
		}
	}
	return port
}
