package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"
)

type SessionMan struct {
	mu sync.Mutex
	md map[[16]byte]*SessionManItem
}

type SessionManItem struct {
	lock          sync.Mutex
	ctx           context.Context
	cancel        context.CancelFunc
	controlCtx    context.Context
	controlCancel context.CancelFunc
	inDataCh      chan []byte
	outDataCh     chan InboundData
	tgtSeqNum     uint8
	pendingData   []byte
}

func NewSessionMan() *SessionMan {
	return &SessionMan{
		md: make(map[[16]byte]*SessionManItem),
	}
}

func (sm *SessionMan) Register(addr string, port uint16, timeout int) ([]byte, error) {
	// Connect
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", addr, port), time.Duration(timeout)*time.Second)
	if err != nil {
		return nil, err
	}

	// Lock map
	sm.mu.Lock()
	defer sm.mu.Unlock()

	// Generate UID
	var uid [16]byte
	isGenerated := false
	for i := 0; i < 100; i++ {
		if _, err = rand.Read(uid[:]); err != nil {
			continue
		}
		if _, ok := sm.md[uid]; ok {
			continue
		}
		isGenerated = true
		break
	}
	if !isGenerated {
		return nil, fmt.Errorf("failed to generate uid")
	}

	// Define channels and context
	var (
		inDataCh  = make(chan []byte, 1)
		outDataCh = make(chan InboundData, 1)
	)
	ctx, cancel := context.WithCancel(context.Background())

	// Store
	sm.md[uid] = &SessionManItem{
		ctx:           ctx,
		cancel:        cancel,
		controlCtx:    nil,
		controlCancel: nil,
		inDataCh:      inDataCh,
		outDataCh:     outDataCh,
		tgtSeqNum:     0,
		pendingData:   nil,
	}

	// Handle connection
	go func() {
		errCh := make(chan error, 10)

		go func() { // reader
			for {
				buf := make([]byte, 4096)
				n, readerErr := conn.Read(buf)
				if readerErr != nil {
					errCh <- readerErr
					return
				}
				if n > 0 {
					select {
					case <-ctx.Done():
						return
					case inDataCh <- buf[:n]:
					}
				}
			}
		}()

		go func() { // writer
			srcSeqNum := uint8(0)
			for {
				select {
				case <-ctx.Done():
					return
				case inboundData := <-outDataCh:
					if srcSeqNum != inboundData.seqNum {
						slog.Debug(fmt.Sprintf("Warning: source sequence numbers do not match (%d != %d)", srcSeqNum, inboundData.seqNum))
						continue
					}
					if _, writerErr := conn.Write(inboundData.data); writerErr != nil {
						errCh <- writerErr
						return
					}
					srcSeqNum++
				}
			}
		}()

		// Handle events
		select {
		case <-ctx.Done():
		case err = <-errCh:
			slog.Debug("Target connection finished", slog.String("err", err.Error()))
		}

		// Finish
		cancel()
		conn.Close()

		// Unregister
		sm.mu.Lock()
		delete(sm.md, uid)
		sm.mu.Unlock()
	}()

	// Return uid
	uidCopy := make([]byte, 16)
	copy(uidCopy, uid[:])
	return uidCopy, nil
}

func (sm *SessionMan) Control(uid []byte) (context.Context, context.CancelFunc, context.CancelFunc, chan []byte, chan InboundData, uint8, []byte, error) {
	// Convert uid
	var tmpUid [16]byte
	copy(tmpUid[:], uid)

	// Find in map
	sm.mu.Lock()
	item, ok := sm.md[tmpUid]
	sm.mu.Unlock()
	if !ok {
		return nil, nil, nil, nil, nil, 0, nil, fmt.Errorf("uid not found")
	}

	// Finish existing control
	if item.controlCancel != nil {
		item.controlCancel()
	}

	// Take new control
	item.lock.Lock()

	// Check maybe connection already finished
	if item.ctx.Err() != nil {
		item.lock.Unlock()
		return nil, nil, nil, nil, nil, 0, nil, fmt.Errorf("connection finished")
	}

	// Make new control context
	controlCtx, controlCancel := context.WithCancel(item.ctx)
	item.controlCtx = controlCtx
	item.controlCancel = controlCancel

	// Return
	return controlCtx, item.cancel, controlCancel, item.inDataCh, item.outDataCh, item.tgtSeqNum, item.pendingData, nil
}

func (sm *SessionMan) Leave(uid []byte, tgtSeqNum uint8, pendingData []byte) {
	// Convert uid
	var tmpUid [16]byte
	copy(tmpUid[:], uid)

	// Find in map
	sm.mu.Lock()
	item, ok := sm.md[tmpUid]
	sm.mu.Unlock()
	if !ok {
		return
	}

	// Store state
	item.controlCtx = nil
	item.controlCancel = nil
	item.tgtSeqNum = tgtSeqNum
	item.pendingData = pendingData

	// Unlock
	item.lock.Unlock()
}
