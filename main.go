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
	"sync"
	"syscall"
	"time"
)

type EventType int

const (
	EventJoined EventType = iota
	EventLeft
	EventSaveQuerySuccess
	EventSaveQueryData
)

var (
	RegexDisconnected = regexp.MustCompile(`Player disconnected: (.*),`)
	RegexConnected    = regexp.MustCompile(`Player connected: (.*),`)
)

type PlayerEvent struct {
	PlayerID string
	RawLine  string
	Event    EventType
}

func (d *EventDetector) detectEvent(line string) (PlayerEvent, bool) {
	if d.nextLineIsSaveQueryData {
		d.nextLineIsSaveQueryData = false
		return PlayerEvent{
			Event:   EventSaveQueryData,
			RawLine: line,
		}, true
	}

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

	trimed := strings.TrimSpace(line)
	if trimed == "Data saved. Files are now ready to be copied." {
		d.nextLineIsSaveQueryData = true
		return PlayerEvent{
			Event:   EventSaveQuerySuccess,
			RawLine: line,
		}, true
	}

	return PlayerEvent{}, false
}

type EventDetector struct {
	EventChan chan PlayerEvent
	ForwardTo io.Writer

	EventForwards    []chan PlayerEvent
	EventForwardLock sync.Mutex

	nextLineIsSaveQueryData bool
}

func (d *EventDetector) EventProcessingLoop(ctx context.Context) {
	log.Printf("started event processing loop")
LOOP:
	for {
		select {
		case event := <-d.EventChan:
			log.Printf("[Starter] Event: %v", event)
			func() {
				d.EventForwardLock.Lock()
				defer d.EventForwardLock.Unlock()
				for _, c := range d.EventForwards {
					c <- event
				}
			}()
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

		event, ok := d.detectEvent(line)
		if !ok {
			continue
		}

		d.EventChan <- event
	}

	return d.ForwardTo.Write(p)
}

func (d *EventDetector) AddEventListener(ch chan PlayerEvent) {
	d.EventForwardLock.Lock()
	defer d.EventForwardLock.Unlock()
	d.EventForwards = append(d.EventForwards, ch)
}

func (d *EventDetector) RemoveEventListener(ch chan PlayerEvent) {
	d.EventForwardLock.Lock()
	defer d.EventForwardLock.Unlock()
	for i, c := range d.EventForwards {
		if c == ch {
			d.EventForwards = append(d.EventForwards[0:i], d.EventForwards[i+1:]...)
			break
		}
	}
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
		log.Printf("wrote command: %v", cmd)
		return err
	}

	nowBackuping := false
	serverStopped := false

	backupSequence := func() error {
		if serverStopped {
			log.Printf("unable to backup (server is already stopped)")
			return nil
		}

		nowBackuping = true
		defer func() {
			nowBackuping = false
		}()

		// add event listener
		listener := make(chan PlayerEvent, 10)
		stdoutDetector.AddEventListener(listener)
		defer func() {
			// consume listener
			for len(listener) > 0 {
				<-listener
			}
			stdoutDetector.RemoveEventListener(listener)
		}()

		// save hold
		if err := writeCommand("save hold"); err != nil {
			return err
		}

		// wait for save query data in another thread
		queryLineChan := func() <-chan string {
			ch := make(chan string, 1)
			go func() {
				// wait for save query success
				for e := range listener {
					if e.Event == EventSaveQuerySuccess {
						break
					}
				}
				// wait for save query data
				line := ""
				for e := range listener {
					if e.Event == EventSaveQueryData {
						line = e.RawLine
						break
					}
				}
				ch <- line
			}()
			return ch
		}()

		// repeat save query until we get save query result
		queryLine := ""
		saveQueryTimer := time.NewTicker(1 * time.Second)
	LOOP:
		for {
			select {
			case line := <-queryLineChan:
				queryLine = line
				saveQueryTimer.Stop()
				break LOOP
			case <-saveQueryTimer.C:
				if err := writeCommand("save query"); err != nil {
					log.Printf("save query failed")
				}
			}
		}

		// TODO: copy files
		log.Printf("queryLine: %v", queryLine)

		// save resume
		if err := writeCommand("save resume"); err != nil {
			return err
		}

		return nil
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
			serverStopped = true
			break LOOP

		case stdin := <-stdinChan:
			if nowBackuping {
				log.Printf("server is backuping now. stdin disabled.")
				break
			}

			if strings.TrimSpace(stdin) == "@backup@" {
				go func() {
					if err := backupSequence(); err != nil {
						log.Printf("error in backup sequence: %v", err)
					}
				}()
				break
			}

			if err := writeCommand(stdin); err != nil {
				log.Printf("write to process stdin failed: %v", err)
			}

		case <-sigs:
			stopSequence := func() {
				serverStopped = true
				log.Printf("stop signal received")
				log.Printf("sending stop command to server...")
				if err := writeCommand("stop"); err != nil {
					log.Printf("send stop failed: %v", err)
				}
			}

			if nowBackuping {
				log.Printf("server is backuping now. signals are sent back after backup finished.")
				go func() {
					timer := time.NewTicker(1 * time.Second)
					for range timer.C {
						if nowBackuping {
							continue
						}

						timer.Stop()
						break
					}
					stopSequence()
				}()
			} else {
				stopSequence()
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
