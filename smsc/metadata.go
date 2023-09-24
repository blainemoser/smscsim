package smsc

import (
	"fmt"
	"sort"
	"sync"
)

type (
	metaData struct {
		smsc      *Smsc
		mu        *sync.Mutex
		sessionId int
		systemId  string
		password  string
		bound     bool
		receiver  bool
		Log       LogMessageChan
		message   MessageChan
	}
)

func NewMetaData(smsc *Smsc, messageChan MessageChan, log LogMessageChan) *metaData {
	return &metaData{
		smsc:     smsc,
		mu:       &sync.Mutex{},
		systemId: DEFAULT_SYSTEM_ID,
		bound:    false,
		receiver: false,
		message:  messageChan,
		Log:      log,
	}
}

func (m *metaData) setSysId(id, password string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.systemId = id
	m.password = password
}

func (m *metaData) setSessionId(id int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessionId = id
}

func (m *metaData) setBound(b bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.bound = b
}

func (m *metaData) setReceiver(r bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.receiver = r
}

func (m *metaData) findSystemIdAndPassword(cstrings map[int]string) error {
	systemId, password := findUsernameAndPassword(cstrings)
	if len(systemId) < 1 {
		return fmt.Errorf("system id not found; empty username")
	}
	m.setSysId(systemId, password)
	return nil
}

func findUsernameAndPassword(cstrings map[int]string) (username, password string) {
	indeces := make([]int, 0)
	for index := range cstrings {
		indeces = append(indeces, index)
	}
	sort.Ints(indeces)
	for i, index := range indeces {
		if i == 0 {
			username = cstrings[index]
			continue
		} else if i == 1 {
			password = cstrings[index]
			continue
		}
		break
	}
	return
}
