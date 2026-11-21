package main

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"runtime"
)

var (
	gConfig     Config
	gTLSConfig  tls.Config
	gSessionMan = NewSessionMan()
)

type Config struct {
	Host     string `json:"host"`
	Timeout  int    `json:"timeout"`
	LogLevel int    `json:"log_level"`
}

type InboundData struct {
	seqNum uint8
	data   []byte
}

func LoadConfig() error {
	// Read config.json
	file, err := os.Open("config.json")
	if err != nil {
		return err
	}
	defer file.Close()

	// Decode config
	err = json.NewDecoder(file).Decode(&gConfig)
	if err != nil {
		return err
	}

	// Read certificates
	cert, err := tls.LoadX509KeyPair("cert.crt", "cert.key")
	if err != nil {
		return err
	}
	gTLSConfig.Certificates = []tls.Certificate{cert}

	return nil
}

func DoRegister(srcConn net.Conn) error {
	// Handle
	tgtAddr, tgtPort, err := STCPHandleRegister(srcConn)
	if err != nil {
		return err
	}

	// Register
	uid, err := gSessionMan.Register(tgtAddr, tgtPort, gConfig.Timeout)
	if err != nil {
		return err
	}

	// Reply
	if err = STCPDoRegisterReply(srcConn, uid); err != nil {
		return err
	}

	return nil
}

func DoControl(srcConn net.Conn) error {
	// Handle
	uid, err := STCPHandleControl(srcConn)
	if err != nil {
		return err
	}

	// Control
	ctx, globalCancel, localCancel, inCh, outCh, tgtSeqNum, pendingData, err := gSessionMan.Control(uid)
	if err != nil {
		STCPDoControlReply(srcConn, 0x00)
		return err
	}

	// Reply success
	if err = STCPDoControlReply(srcConn, 0x01); err != nil {
		return err
	}

	// Exchange
	var (
		errCh = make(chan error, 10)
		ackCh = make(chan struct{}, 1)
	)

	go func() { // writer
		for {
			// Read from target
			if pendingData == nil {
				select {
				case <-ctx.Done():
					return
				case pendingData = <-inCh:
				}
			}

			// Write to source
			if writerErr := STCPDoPush(srcConn, tgtSeqNum, pendingData, uint16(len(pendingData))); writerErr != nil {
				errCh <- writerErr
				return
			}

			// Wait for ACK
			select {
			case <-ctx.Done():
				return
			case <-ackCh:
				tgtSeqNum++
				pendingData = nil
			}
		}
	}()

	go func() { // reader
		for {
			// Get opcode
			opcode, readerErr := STCPGetOpCode(srcConn)
			if readerErr != nil {
				errCh <- readerErr
				return
			}

			switch opcode {
			case 0x03: // PUSH
				// Handle
				recSrcSeqNum, data, readerErr := STCPHandlePush(srcConn)
				if readerErr != nil {
					errCh <- readerErr
					return
				}

				// Write
				select {
				case <-ctx.Done():
					return
				case outCh <- InboundData{recSrcSeqNum, data}:
				}

				// Confirm
				if readerErr := STCPDoPushAck(srcConn, recSrcSeqNum); readerErr != nil {
					errCh <- readerErr
					return
				}
			case 0x04: // PUSH ACK
				// Handle
				recTgtSeqNum, readerErr := STCPHandlePushAck(srcConn)
				if readerErr != nil {
					errCh <- readerErr
					return
				}

				// Compare sequences
				if tgtSeqNum != recTgtSeqNum {
					errCh <- fmt.Errorf("target sequence numbers do not match (%d != %d)", tgtSeqNum, recTgtSeqNum)
				}

				// Confirm
				select {
				case <-ctx.Done():
					return
				case ackCh <- struct{}{}:
				}
			case 0x05: // FINISH
				globalCancel()
				return
			default:
				errCh <- fmt.Errorf("unknown opcode (%d)", opcode)
				return
			}
		}
	}()

	// Handle events
	select {
	case <-ctx.Done():
		STCPDoFinish(srcConn)
	case err = <-errCh:
		slog.Debug("Source connection finished", slog.String("err", err.Error()))
	}

	// Shutdown
	localCancel()
	srcConn.Close()

	// Leave control
	gSessionMan.Leave(uid, tgtSeqNum, pendingData)

	// Exit
	return nil
}

func HandleSession(srcConn net.Conn) {
	// Handle current session
	RemoteAddr := srcConn.RemoteAddr().String()
	slog.Info("New session",
		slog.String("RemoteAddr", RemoteAddr),
		slog.Int("NumGoroutine", runtime.NumGoroutine()),
	)
	defer func() {
		slog.Info("Session finished",
			slog.String("RemoteAddr", RemoteAddr),
			slog.Int("NumGoroutine", runtime.NumGoroutine()),
		)
		srcConn.Close()
	}()

	// Iterate over packets
	for {
		opcode, err := STCPGetOpCode(srcConn)
		if err != nil {
			slog.Error("Failed to get opcode", slog.String("RemoteAddr", RemoteAddr), slog.String("error", err.Error()))
			return
		}

		switch opcode {
		case 0x01: // REGISTER
			if err := DoRegister(srcConn); err != nil {
				slog.Error("Failed to register connection", slog.String("RemoteAddr", RemoteAddr), slog.String("err", err.Error()))
				return
			}
		case 0x02: // CONTROL
			if err := DoControl(srcConn); err != nil {
				slog.Error("Failed to control connection", slog.String("RemoteAddr", RemoteAddr), slog.String("err", err.Error()))
				return
			}
			return
		default:
			slog.Error("Received unknown opcode", slog.String("RemoteAddr", RemoteAddr), slog.String("opcode", string(opcode)))
			return
		}
	}
}

func main() {
	// Load config
	err := LoadConfig()
	if err != nil {
		slog.Error("Failed to load config", slog.String("err", err.Error()))
		os.Exit(1)
	}

	// Set logging level
	slog.SetLogLoggerLevel(slog.Level(gConfig.LogLevel))

	// Setup listener
	listener, err := tls.Listen("tcp", gConfig.Host, &gTLSConfig)
	if err != nil {
		slog.Error("Failed to setup listener", slog.String("err", err.Error()))
		os.Exit(1)
	}
	defer listener.Close()

	// Wait for connections
	slog.Info("Server started and ready to accept connections", slog.String("host", listener.Addr().String()))
	for {
		conn, err := listener.Accept()
		if err != nil {
			slog.Error("Failed to accept connection", slog.String("err", err.Error()))
			continue
		}

		go HandleSession(conn)
	}
}
