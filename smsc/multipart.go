package smsc

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"sync"
)

type (
	multipart struct {
		id       string
		total    int
		current  int
		src      string
		dst      string
		messages map[int]*multipartMessage
	}

	multiparts struct {
		mu    *sync.Mutex
		parts map[string]*multipart
	}

	multipartMessage struct {
		src            string
		dst            string
		signatureBytes []byte
		page           int
		pages          int
		chars          []byte
	}
)

func newMultiparts() *multiparts {
	return &multiparts{
		mu:    &sync.Mutex{},
		parts: make(map[string]*multipart),
	}
}

func (m *multiparts) clear(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.parts, id)
}

// handles multipart messages
// Headers are as follows:
// 0 length of the PDU header; this is 5 or 6 usually
// 1 Indicator that this is multipart - this should be 8
// 2 length of the subheader
// 3 id of the message; parts should share this
// 4 second id of the message; parts should share this
// 5 The total number of parts
// 6 Which number part this is
func (m *multiparts) handle(messageBody []byte, src, dest string) (multi *multipart, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if len(messageBody) < 6 {
		return nil, fmt.Errorf("message body is only %d character bytes", len(messageBody))
	}
	if messageBody[1] != 8 {
		return nil, fmt.Errorf("message body signature expected 8, got %d", messageBody[1])
	}

	subHeaderLength := int(messageBody[2])
	if subHeaderLength < 3 {
		return nil, fmt.Errorf("sub header length is less than three (%d)", subHeaderLength)
	}
	subHeaders := messageBody[3 : 3+subHeaderLength]
	if len(subHeaders) != subHeaderLength {
		return nil, fmt.Errorf("actual length of sub headers length is %d, expected %d", len(subHeaders), subHeaderLength)
	}
	msgSignature := make([]byte, 0)
	for len(subHeaders) > 2 {
		msgSignature = append(msgSignature, subHeaders[0])
		subHeaders = subHeaders[1:]
	}
	pages := subHeaders[0]
	page := subHeaders[1]
	multi = m.upsert(&multipartMessage{
		src:            src,
		dst:            dest,
		signatureBytes: msgSignature,
		chars:          messageBody[3+subHeaderLength:],
		page:           int(page),
		pages:          int(pages),
	})
	return
}

func (m *multipartMessage) signature() string {
	return getMD5Hash(m.src, m.dst, m.signatureBytes)
}

func (m *multiparts) upsert(message *multipartMessage) (multi *multipart) {
	sig := message.signature()
	var ok bool
	if multi, ok = m.parts[sig]; !ok {
		multi = &multipart{
			messages: make(map[int]*multipartMessage),
		}
		m.parts[sig] = multi
		multi.id = sig
		multi.total = message.pages
		multi.src = message.src
		multi.dst = message.dst
	}
	multi.current = message.page
	multi.messages[message.page] = message
	return
}

func (m *multipart) ready() bool {
	return m.total == m.current
}

func (m *multipart) getMessage() MO {
	message := PDUMessage{
		MsgSourceAddr:      m.src,
		MsgDestinationAddr: m.dst,
		MsgCommand:         SUBMIT_SM,
		MsgResponse:        []byte{},
		MsgID:              "",
		Message:            m.getMessages(),
	}
	return MO{message}
}

func (m *multipart) getMessages() string {
	var message string
	for i := 0; i < len(m.messages); i++ {
		message += string(m.messages[i+1].chars)
	}
	return message
}

func getMD5Hash(source, dest string, signature []byte) string {
	hash := md5.Sum([]byte(source + dest + string(signature)))
	return hex.EncodeToString(hash[:])
}
