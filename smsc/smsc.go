package smsc

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"log"
	"math/rand"
	"net"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"
)

// command id
const (
	GENERIC_NACK      = 0x80000000
	BIND_RECEIVER     = 0x00000001
	BIND_TRANSMITTER  = 0x00000002
	BIND_TRANSCEIVER  = 0x00000009
	SUBMIT_SM         = 0x00000004
	SUBMIT_SM_RESP    = 0x80000004
	DELIVER_SM        = 0x00000005
	DELIVER_SM_RESP   = 0x80000005
	UNBIND            = 0x00000006
	UNBIND_RESP       = 0x80000006
	ENQUIRE_LINK      = 0x00000015
	ENQUIRE_LINK_RESP = 0x80000015

	// command status
	STS_OK            = 0x00000000
	STS_INVALID_CMD   = 0x00000003
	STS_INV_BIND_STS  = 0x00000004
	STS_ALREADY_BOUND = 0x00000005

	// data coding
	CODING_DEFAULT = 0x00
	CODING_UCS2    = 0x08

	// optional parameters
	TLV_RECEIPTED_MSG_ID = 0x001E
	TLV_MESSAGE_STATE    = 0x0427

	// types
	MOBILE_ORIGINATED = "mobile_originated"
	DLR               = "delivery_report"
	MULTIPART         = "multipart"
)

var (
	nullTerm          = []byte("\x00")
	multipartMessages *multiparts
	pendingDeliveries *deliveries
)

type (

	// Channel for sending messages so that your app can deal with them
	MessageChan chan Message

	// Main Smsc Object
	Smsc struct {
		mu        *sync.Mutex
		Sessions  map[int]Session
		BlockDLRs bool // this is because I need to stress test software where DLRs fail to get sent; it's most likely not going to be useful
	}

	//
	Session struct {
		SystemId  string
		Conn      net.Conn
		ReceiveMo bool
	}

	// Your app needs to handle log messages; these will be sent on a log message channel
	// Your app must also handle errors which will be sent on an error channel
	LogMessage struct {
		Level   string
		Message string
	}

	LogMessageChan chan LogMessage

	Tlv struct {
		Tag   int
		Len   int
		Value []byte
	}

	Message interface {
		SourceAddr() string
		DestinationAddr() string
		Command() uint32
		MessageReceived() string
		// SentOrReceived() bool // true for sent, false for received
		MessageId() string
		Response() []byte
		Type() string
	}

	PDUMessage struct {
		MsgSourceAddr      string
		MsgDestinationAddr string
		MsgCommand         uint32
		MsgResponse        []byte
		MsgID              string
		Message            string
		dlrID              uint32
	}

	DeliveryReport struct {
		PDUMessage
	}

	MO struct {
		PDUMessage
	}
)

func (msg PDUMessage) SourceAddr() string {
	return msg.MsgSourceAddr
}

func (msg PDUMessage) DestinationAddr() string {
	return msg.MsgDestinationAddr
}

func (msg PDUMessage) Command() uint32 {
	return msg.MsgCommand
}

func (msg PDUMessage) MessageId() string {
	return msg.MsgID
}

func (msg PDUMessage) MessageReceived() string {
	return msg.Message
}

func (msg MO) Type() string {
	return MOBILE_ORIGINATED
}

func (msg DeliveryReport) Type() string {
	return DLR
}

func (msg PDUMessage) Response() []byte {
	return msg.MsgResponse
}

func NewSmsc() Smsc {
	sessions := make(map[int]Session)
	return Smsc{Sessions: sessions, mu: &sync.Mutex{}, BlockDLRs: false}
}

func (smsc *Smsc) Start(port int, stopChan chan struct{}, message MessageChan, logChan LogMessageChan) error {
	ln, err := net.Listen("tcp", fmt.Sprint(":", port))
	if err != nil {
		return err
	}

	logChan <- LogMessage{Level: "info", Message: fmt.Sprintf("SMSC simulator listening on port %d", port)}
	var conn net.Conn
	defer func() {
		logChan <- LogMessage{Level: "info", Message: "Closing connection"}
		ln.Close()
		if conn != nil {
			conn.Close()
		}
	}()
running:
	for {
		select {
		case <-stopChan:
			logChan <- LogMessage{Level: "info", Message: "received signal to stop SMSC Server"}
			break running
		default:
			conn, err = ln.Accept()
			if err != nil {
				logChan <- LogMessage{Level: "error", Message: fmt.Sprintf("error accepting new tcp connection %v", err)}
			} else {
				go handleSmppConnection(smsc, conn, message, logChan)
			}
		}
	}
	return nil
}

func (smsc *Smsc) BoundSystemIds() []string {
	smsc.mu.Lock()
	defer smsc.mu.Unlock()
	var systemIds []string
	for _, sess := range smsc.Sessions {
		systemId := sess.SystemId
		systemIds = append(systemIds, systemId)
	}
	return systemIds
}

func (smsc *Smsc) SendMoMessage(sender, recipient, message, systemId string) (MO, error) {
	smsc.mu.Lock()
	defer smsc.mu.Unlock()
	var session *Session = nil
	for _, sess := range smsc.Sessions {
		if systemId == sess.SystemId {
			session = &sess
			break
		}
	}

	if session == nil {
		return MO{}, fmt.Errorf("No session found for systemId: [%s]", systemId)
	}

	if !session.ReceiveMo {
		return MO{}, fmt.Errorf("Only RECEIVER and TRANSCEIVER sessions could receive MO messages")
	}

	// TODO implement UDH for large messages
	// shortMsg := truncateString(message, 70) // just truncate to 70 symbols
	var tlvs []Tlv
	pdumsg := PDUMessage{
		MsgSourceAddr:      sender,
		MsgDestinationAddr: recipient,
		MsgResponse:        []byte(message),
		MsgID:              strconv.Itoa(int(rand.Int31())),
	}
	if len(pdumsg.MsgResponse) > int(maxCharLen) {
		if err := SendLongMessageParts(pdumsg, session); err != nil {
			return MO{}, err
		}
		return MO{pdumsg}, nil
	}
	dlrId := rand.Uint32()
	pendingDeliveries.insert(dlrId, pdumsg)
	data := deliverSmPDU(pdumsg, CODING_DEFAULT, dlrId, tlvs, "")
	if _, err := session.Conn.Write(data); err != nil {
		log.Printf("Cannot send MO message to systemId: [%s]. Network error [%v]", systemId, err)
		return MO{}, err
	}
	// log.Printf("MO message to systemId: [%s] was successfully sent. Sender: [%s], recipient: [%s]", systemId, sender, recipient)
	return MO{pdumsg}, nil
}

// how to convert ints to and from bytes https://golang.org/pkg/encoding/binary/

func handleSmppConnection(smsc *Smsc, conn net.Conn, message MessageChan, logHandler LogMessageChan) {

	multipartMessages = newMultiparts()
	pendingDeliveries = NewDeliveries()
	meta := NewMetaData(smsc, message, logHandler)
	defer func() {
		smsc.mu.Lock()
		defer smsc.mu.Unlock()
		delete(smsc.Sessions, meta.sessionId)
	}()
	defer conn.Close()

	for {
		commander, err := NewCommand(meta, conn)
		if err != nil {
			logHandler <- LogMessage{Level: "error", Message: err.Error()}
			break
		}
		respBytes, err := commander.instruction().handle()
		if err != nil {
			logHandler <- LogMessage{Level: "error", Message: err.Error()}
			break
		}
		if _, err := conn.Write(respBytes); err != nil {
			logHandler <- LogMessage{Level: "error", Message: err.Error()}
			return
		}
	}
}

const DELIVERY_RECEIPT_FORMAT = "id:%s sub:001 dlvrd:001 submit date:%s done date:%s stat:DELIVRD err:000 Text:"

func deliveryReceiptPDU(message PDUMessage, submitDate, doneDate time.Time) (PDUMessage, []byte) {
	message.Message = ""

	// Flip the source and dest numbers around; some platforms expect this.
	source := message.MsgDestinationAddr
	destination := message.MsgSourceAddr

	message.MsgSourceAddr = source
	message.MsgDestinationAddr = destination

	sbtDateFrmt := submitDate.Format("0601021504")
	doneDateFrmt := doneDate.Format("0601021504")
	deliveryReceipt := fmt.Sprintf(DELIVERY_RECEIPT_FORMAT, message.MessageId(), sbtDateFrmt, doneDateFrmt)
	var tlvs []Tlv

	// receipted_msg_id TLV
	var rcptMsgIdBuf bytes.Buffer
	rcptMsgIdBuf.WriteString(message.MessageId())
	rcptMsgIdBuf.WriteByte(0) // null terminator
	receiptMsgId := Tlv{TLV_RECEIPTED_MSG_ID, rcptMsgIdBuf.Len(), rcptMsgIdBuf.Bytes()}
	tlvs = append(tlvs, receiptMsgId)

	// message_state TLV
	msgStateTlv := Tlv{TLV_MESSAGE_STATE, 1, []byte{2}} // 2 - delivered
	tlvs = append(tlvs, msgStateTlv)

	message.MsgResponse = []byte(deliveryReceipt)

	dlrID := rand.Uint32()
	data := deliverSmPDU(message, CODING_DEFAULT, dlrID, tlvs, DLR)
	return message, data
}

func deliverSmPDU(message PDUMessage, coding byte, seqNum uint32, tlvs []Tlv, msgType string) []byte {
	// header without cmd_len
	header := make([]byte, 12)
	binary.BigEndian.PutUint32(header[0:], uint32(DELIVER_SM))
	binary.BigEndian.PutUint32(header[4:], uint32(0))
	binary.BigEndian.PutUint32(header[8:], uint32(seqNum)) // rand seq num

	// pdu body buffer
	var buf bytes.Buffer
	buf.Write(header)

	buf.WriteString("smscsim")
	buf.WriteByte(0) // null term

	buf.WriteByte(0) // src ton
	buf.WriteByte(0) // src npi
	if message.SourceAddr() == "" {
		buf.WriteByte(0)
	} else {
		buf.WriteString(message.SourceAddr())
		buf.WriteByte(0)
	}

	buf.WriteByte(0) // dest ton
	buf.WriteByte(0) // dest npi
	if message.DestinationAddr() == "" {
		buf.WriteByte(0)
	} else {
		buf.WriteString(message.DestinationAddr())
		buf.WriteByte(0)
	}
	if msgType == DLR {
		buf.WriteString(string([]byte{4})) // esm class 4
	} else if msgType == MULTIPART {
		buf.WriteByte(0x40) // esm class
	} else {
		buf.WriteByte(0) // esm class
	}
	buf.WriteByte(0)      // protocol id
	buf.WriteByte(0)      // priority flag
	buf.WriteByte(0)      // sched delivery time
	buf.WriteByte(0)      // validity period
	buf.WriteByte(0)      // registered delivery
	buf.WriteByte(0)      // replace if present
	buf.WriteByte(coding) // data coding
	buf.WriteByte(0)      // def msg id

	smLen := len(message.Response())
	buf.WriteByte(byte(smLen))
	buf.Write(message.Response())

	for _, t := range tlvs {
		tlvBytes := make([]byte, 4)
		binary.BigEndian.PutUint16(tlvBytes[0:], uint16(t.Tag))
		binary.BigEndian.PutUint16(tlvBytes[2:], uint16(t.Len))
		buf.Write(tlvBytes)
		buf.Write(t.Value)
	}

	// calc cmd lenth and append to the begining
	cmdLen := buf.Len() + 4 // +4 for cmdLen field itself
	cmdLenBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(cmdLenBytes[0:], uint32(cmdLen))

	var deliverSm bytes.Buffer
	deliverSm.Write(cmdLenBytes)
	deliverSm.Write(buf.Bytes())

	return deliverSm.Bytes()
}

func truncateString(input string, maxLen int) string {
	result := input
	if len(input) > maxLen {
		result = input[0:maxLen]
	}
	return result
}

func toUcs2Coding(input string) []byte {
	// not most elegant implementation, but ok for testing purposes
	l := utf8.RuneCountInString(input)
	buf := make([]byte, l*2) // two bytes per character
	idx := 0
	for len(input) > 0 {
		r, s := utf8.DecodeRuneInString(input)
		if r <= 65536 {
			binary.BigEndian.PutUint16(buf[idx:], uint16(r))
		} else {
			binary.BigEndian.PutUint16(buf[idx:], uint16(63)) // question mark
		}
		input = input[s:]
		idx += 2
	}
	return buf
}
