package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"regexp"
	"strings"
	"syscall"
)

type EventType int

const (
	EventJoined EventType = iota
	EventLeft
)

var (
	RegexDisconnected = regexp.MustCompile(`Player disconnected: (.*),`)
	RegexConnected    = regexp.MustCompile(`Player connected: (.*),`)
)

type PlayerEvent struct {
	PlayerID string
	Event    EventType
}

func detectEvent(line string) (PlayerEvent, bool) {
	matched := RegexConnected.FindStringSubmatch(line)
	if matched != nil {
		return PlayerEvent{
			PlayerID: matched[1],
			Event:    EventJoined,
		}, true
	}

	matched = RegexDisconnected.FindStringSubmatch(line)
	if matched != nil {
		return PlayerEvent{
			PlayerID: matched[1],
			Event:    EventLeft,
		}, true
	}

	return PlayerEvent{}, false
}

type EventDetector struct {
	EventChan chan PlayerEvent
	ForwardTo io.Writer
}

func (d *EventDetector) EventProcessingLoop(ctx context.Context) {
	log.Printf("started event processing loop")
LOOP:
	for {
		select {
		case event := <-d.EventChan:
			log.Printf("[Starter] Event: %v", event)
		case <-ctx.Done():
			log.Printf("[Starter] stopping event processing loop")
			break LOOP
		}
	}
}

func (d *EventDetector) Write(p []byte) (int, error) {
	for _, line := range strings.Split(string(p), "\n") {
		if line == "" {
			continue
		}

		event, ok := detectEvent(line)
		if !ok {
			continue
		}

		d.EventChan <- event
	}

	return d.ForwardTo.Write(p)
}

func cmdRunner(cmd *exec.Cmd) <-chan error {
	ch := make(chan error, 1)
	go func() {
		err := cmd.Run()
		log.Printf("cmd.Run() done sending signal...")
		ch <- err
	}()
	return ch
}

func stdinGetter() <-chan string {
	ch := make(chan string, 10)

	scanner := bufio.NewScanner(os.Stdin)
	go func() {
		for {
			if scanner.Scan() {
				line := scanner.Text()
				ch <- line
			} else {
				close(ch)
				return
			}
		}
	}()

	return ch
}

// cmdExitNotifier waits for the process to exit and emits signal
// func cmdExitNotifier(cmd *exec.Cmd) <-chan struct{} {
// 	ch := make(chan struct{}, 1)
// 	go func() {
// 		if err := cmd.Wait(); err != nil {
// 			log.Printf("cmd.Wait error: %v", err)
// 		}
// 		ch <- struct{}{}
// 	}()
// 	return ch
// }

func main() {
	exePath := os.Getenv("EXE_PATH")
	if exePath == "" {
		log.Fatal("EXE_PATH is not set")
		return
	}

	dirname := path.Dir(exePath)
	if err := os.Chdir(dirname); err != nil {
		log.Fatalf("chdir failed: %v", err)
		return
	}

	eventChan := make(chan PlayerEvent, 100)
	stdoutDetector := &EventDetector{
		EventChan: eventChan,
		ForwardTo: os.Stdout,
	}

	ctx, cancelFunc := context.WithCancel(context.Background())
	go stdoutDetector.EventProcessingLoop(ctx)

	filename := "./" + path.Base(exePath)
	cmd := exec.Command(filename)
	cmd.Stdout = stdoutDetector
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		log.Fatalf("could not open stdin")
	}
	log.Printf("Server starting...")

	writeCommand := func(cmd string) error {
		_, err := fmt.Fprintf(stdin, "%s\n", cmd)
		return err
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	// if err := cmd.Start(); err != nil {
	// 	log.Fatalf("failed start process %v", err)
	// }

	// exitNotifier := cmdExitNotifier(cmd)

	cmdResultChan := cmdRunner(cmd)
	stdinChan := stdinGetter()
LOOP:
	for {
		select {
		case err := <-cmdResultChan:
			if err != nil {
				log.Printf("cmd result: %v", err)
			}
			log.Println("detected process exit")
			break LOOP

		case stdin := <-stdinChan:
			if err := writeCommand(stdin); err != nil {
				log.Printf("write to process stdin failed: %v", err)
			}

		case <-sigs:
			log.Printf("stop signal received")
			log.Printf("sending stop command to server...")
			if err := writeCommand("stop"); err != nil {
				log.Printf("send stop failed: %v", err)
			}

			// case <-exitNotifier:
			// 	log.Printf("detected process exit")
			// 	break LOOP
		}
	}

	// log.Printf("waiting for process to exit")
	// if err := cmd.Wait(); err != nil {
	// 	log.Printf("process wait failed: %v", err)
	// }

	log.Printf("Server gracefully stopped")
	cancelFunc()
}
