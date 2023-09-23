package smsc

import "sync"

type (
	deliveries struct {
		mu      *sync.Mutex
		reports map[uint32]PDUMessage
	}
)

func NewDeliveries() *deliveries {
	return &deliveries{
		mu:      &sync.Mutex{},
		reports: make(map[uint32]PDUMessage),
	}
}

func (d *deliveries) insert(id uint32, msg PDUMessage) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.reports[id] = msg
}

func (d *deliveries) pop(id uint32) *PDUMessage {
	d.mu.Lock()
	defer d.mu.Unlock()
	if msg, ok := d.reports[id]; ok {
		return &msg
	}
	return nil
}
