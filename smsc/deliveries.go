package smsc

import "sync"

type (
	deliveries struct {
		mu      *sync.Mutex
		reports map[uint32]string
	}
)

func NewDeliveries() *deliveries {
	return &deliveries{
		mu:      &sync.Mutex{},
		reports: make(map[uint32]string),
	}
}
