package smsc

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"strconv"
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
)

// command status

const (
	STS_OK            = 0x00000000
	STS_INVALID_CMD   = 0x00000003
	STS_INV_BIND_STS  = 0x00000004
	STS_ALREADY_BOUND = 0x00000005
)

// data coding

const (
	CODING_DEFAULT = 0x00
	CODING_UCS2    = 0x08
)

// optional parameters

const (
	TLV_RECEIPTED_MSG_ID = 0x001E
	TLV_MESSAGE_STATE    = 0x0427
)

// types

const (
	MOBILE_ORIGINATED = "mobile_originated"
	DLR               = "delivery_report"
)

var (
	nullTerm = []byte("\x00")
)

type Session struct {
	SystemId  string
	Conn      net.Conn
	ReceiveMo bool
}

type Tlv struct {
	Tag   int
	Len   int
	Value []byte
}

type Message interface {
	SourceAddr() string
	DestinationAddr() string
	Command() uint32
	MessageReceived() string
	// SentOrReceived() bool // true for sent, false for received
	MessageId() string
	Response() []byte
	Type() string
}

type PDUMessage struct {
	MsgSourceAddr      string
	MsgDestinationAddr string
	MsgCommand         uint32
	MsgResponse        []byte
	MsgID              string
	Message            string
}

type DeliveryRepoort struct {
	PDUMessage
}

type MO struct {
	PDUMessage
}

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

func (msg DeliveryRepoort) Type() string {
	return DLR
}

func (msg PDUMessage) Response() []byte {
	return msg.MsgResponse
}

type MessageChan chan Message

type Smsc struct {
	Sessions map[int]Session
}

func NewSmsc() Smsc {
	sessions := make(map[int]Session)
	return Smsc{sessions}
}

func (smsc *Smsc) Start(port int, message MessageChan) {
	ln, err := net.Listen("tcp", fmt.Sprint(":", port))
	if err != nil {
		log.Panic(err)
	}
	defer ln.Close()

	log.Println("SMSC simulator listening on port", port)
	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("error accepting new tcp connection %v", err)
		} else {
			go handleSmppConnection(smsc, conn, message)
		}
	}
}

func (smsc *Smsc) BoundSystemIds() []string {
	var systemIds []string
	for _, sess := range smsc.Sessions {
		systemId := sess.SystemId
		systemIds = append(systemIds, systemId)
	}
	return systemIds
}

func (smsc *Smsc) SendMoMessage(sender, recipient, message, systemId string) (string, error) {
	var session *Session = nil
	for _, sess := range smsc.Sessions {
		if systemId == sess.SystemId {
			session = &sess
			break
		}
	}

	if session == nil {
		log.Printf("Cannot send MO message to systemId: [%s]. No bound session found", systemId)
		return "", fmt.Errorf("No session found for systemId: [%s]", systemId)
	}

	if !session.ReceiveMo {
		log.Printf("Cannot send MO message to systemId: [%s]. Only RECEIVER and TRANSCEIVER sessions could receive MO messages", systemId)
		return "", fmt.Errorf("Only RECEIVER and TRANSCEIVER sessions could receive MO messages")
	}

	// TODO implement UDH for large messages
	shortMsg := truncateString(message, 70) // just truncate to 70 symbols
	var tlvs []Tlv
	pdumsg := PDUMessage{
		MsgSourceAddr:      sender,
		MsgDestinationAddr: recipient,
		MsgResponse:        []byte(shortMsg),
		MsgID:              strconv.Itoa(rand.Int()),
	}
	data := deliverSmPDU(pdumsg, CODING_UCS2, rand.Int(), tlvs, false)
	if _, err := session.Conn.Write(data); err != nil {
		log.Printf("Cannot send MO message to systemId: [%s]. Network error [%v]", systemId, err)
		return "", err
	}
	log.Printf("MO message to systemId: [%s] was successfully sent. Sender: [%s], recipient: [%s]", systemId, sender, recipient)
	return pdumsg.MessageId(), nil
}

// According to the SMPP spec, cstrings demarcate
// several sections of the PDU response will contain c-strings.
// The purpose of this function is to index these c-strings by
// their index positions within the body as a whole.
// Note that the index positons here will be the PDU body
// excluding the header; in other words, excluding he command
// which should be parsed out before.
func extractCStrings(data []byte) map[int]string {
	cStrings := make(map[int]string)
	var currentString []byte
	var currentIndex int

	for indexPosition, b := range data {
		if b != 0 {
			if len(currentString) < 1 {
				currentIndex = indexPosition
			}
			currentString = append(currentString, b)
		} else if len(currentString) > 0 {
			cStrings[currentIndex] = string(currentString)
			currentString = nil // Reset the current string
		}
	}

	// If the last string is not null-terminated, add it
	if len(currentString) > 0 {
		cStrings[currentIndex] = string(currentString)
	}

	return cStrings
}

// how to convert ints to and from bytes https://golang.org/pkg/encoding/binary/

func handleSmppConnection(smsc *Smsc, conn net.Conn, message MessageChan) {
	sessionId := rand.Int()
	systemId := "anonymous"
	bound := false
	receiver := false

	defer delete(smsc.Sessions, sessionId)
	defer conn.Close()

	for {
		command := make([]byte, 2048) // this is overkill, but it's safe; we'll truncate it. Since it's not a phone we can be a little liberal
		if _, err := conn.Read(command); err != nil && err != io.EOF {
			log.Printf("closing connection for system_id[%s] due %v\n", systemId, err)
			return
		}
		command = bytes.TrimRight(command, "\x00")
		pduHeadBuf := command[:16]
		// cmdLen := binary.BigEndian.Uint32(pduHeadBuf[0:])
		cmdId := binary.BigEndian.Uint32(pduHeadBuf[4:])
		// cmdSts := binary.BigEndian.Uint32(pduHeadBuf[8:])
		seqNum := binary.BigEndian.Uint32(pduHeadBuf[12:])
		// fmt.Println("len", cmdLen, "id", cmdId, "status", cmdSts, "sqnum", seqNum)

		var rmain []byte
		if len(command) >= 16 {
			rmain = command[16:]
		} else {
			rmain = command
		}

		var respBytes []byte

		switch cmdId {
		case BIND_RECEIVER, BIND_TRANSMITTER, BIND_TRANSCEIVER: // bind requests
			{
				// pduBody := make([]byte, cmdLen-16)
				// if _, err := io.ReadFull(conn, pduBody); err != nil {
				// 	log.Printf("closing connection due %v\n", err)
				// 	return/
				// }

				if len(command) < 16 {
					log.Printf("closing connection due %v\n", fmt.Errorf("command not found"))
					return
				}

				cstrings := extractCStrings(command[16:]) // this is a hacky way to fix the bug

				if len(cstrings) < 2 {
					log.Printf("closing connection due %v\n", fmt.Errorf("command not found"))
					return
				}

				for _, value := range cstrings {
					// password would be the second one
					systemId = value
					break
				}

				if len(systemId) < 1 {
					log.Printf("closing connection due %v\n", fmt.Errorf("system ID empty"))
					return
				}
				log.Printf("bind request from system_id[%s]\n", systemId)

				respCmdId := 2147483648 + cmdId // hack to calc resp cmd id

				if bound {
					respBytes = headerPDU(respCmdId, STS_ALREADY_BOUND, seqNum)
					log.Printf("[%s] already has bound session", systemId)
				} else {
					receiveMo := cmdId == BIND_RECEIVER || cmdId == BIND_TRANSCEIVER
					smsc.Sessions[sessionId] = Session{systemId, conn, receiveMo}
					respBytes = stringBodyPDU(respCmdId, STS_OK, seqNum, "smscsim")
					bound = true
					receiver = cmdId == BIND_RECEIVER
				}
			}
		case UNBIND: // unbind request
			{
				log.Printf("unbind request from system_id[%s]\n", systemId)
				respBytes = headerPDU(UNBIND_RESP, STS_OK, seqNum)
				bound = false
				systemId = "anonymous"
			}
		case ENQUIRE_LINK: // enquire_link
			{
				log.Printf("enquire_link from system_id[%s]\n", systemId)
				respBytes = headerPDU(ENQUIRE_LINK_RESP, STS_OK, seqNum)
			}
		case SUBMIT_SM: // submit_sm
			{
				// pduBody := make([]byte, cmdLen-16)
				// if _, err := io.ReadFull(conn, pduBody); err != nil {
				// 	log.Printf("error reading submit_sm body for %s due %v. closing connection", systemId, err)
				// 	return
				// }
				if receiver {
					respBytes = headerPDU(SUBMIT_SM_RESP, STS_INV_BIND_STS, seqNum)
					log.Printf("error handling submit_sm from system_id[%s]. session with bind type RECEIVER cannot send requests", systemId)
					break
				}

				var sourceAddress string
				var destinationAddress string
				var srvType string
				var scheduleEnd string
				var validityEnd string
				var ok bool

				// https://smpp.org/
				pduBody := rmain
				cstrings := extractCStrings(pduBody)
				idxCounter := 0

				// service_type Var.
				// Max 6
				// C-Octet
				// String
				// Indicates the type of service associated with the
				// message.
				// Where not required this should be set to a single
				// NULL byte.
				if srvType, ok = cstrings[idxCounter]; !ok {
					if bytes.Index(pduBody[idxCounter:], nullTerm) == -1 {
						respBytes = headerPDU(GENERIC_NACK, STS_INVALID_CMD, seqNum)
						break
					}
				}
				idxCounter += len(srvType) + 1 // NOW AT Source TON we're not using the service type for now, but it should be in the c-strings if needed
				idxCounter += 2                // NOW AT Source Address; skip npi. if needed, these should be at the following 2 positions after the last service character

				// srcAddrEndIdx := bytes.Index(pduBody[idxCounter:], nullTerm)
				if sourceAddress, ok = cstrings[idxCounter]; !ok {
					respBytes = headerPDU(GENERIC_NACK, STS_INVALID_CMD, seqNum)
					break
				}
				idxCounter += len(sourceAddress) + 1 // move up one index position after source address; NOW AT Destination TON
				idxCounter = idxCounter + 2          // skip dest ton and npi. if needed, they will be at the two positions after the last source address character

				// destAddrEndIdx := bytes.Index(pduBody[idxCounter:], nullTerm)
				if destinationAddress, ok = cstrings[idxCounter]; !ok {
					respBytes = headerPDU(GENERIC_NACK, STS_INVALID_CMD, seqNum)
					break
				}
				idxCounter += len(destinationAddress) + 1 // move up one index after the destination address
				idxCounter = idxCounter + 3               // skip esm_class, protocol_id, priority_flag

				if scheduleEnd, ok = cstrings[idxCounter]; !ok {
					if bytes.Index(pduBody[idxCounter:], nullTerm) == -1 {
						respBytes = headerPDU(GENERIC_NACK, STS_INVALID_CMD, seqNum)
						break
					}
				}
				idxCounter += len(scheduleEnd) + 1 // one char after the schedule end c-string; next is validity period

				if validityEnd, ok = cstrings[idxCounter]; !ok {
					if bytes.Index(pduBody[idxCounter:], nullTerm) == -1 {
						respBytes = headerPDU(GENERIC_NACK, STS_INVALID_CMD, seqNum)
						break
					}
				}
				idxCounter += len(validityEnd) + 1   // counter should now be on the register DLR flag
				registeredDlr := pduBody[idxCounter] // registered_delivery is next field after the validity_period

				// we're not too interested in the fields between the "register DLR" flag and the actual text of the message.
				// Therefore, if we have the space, we'll check for text from the remaining cstring (it would be the last
				// possible one) and use that
				var textMessage string
				if len(pduBody) >= idxCounter+4 {
					idxCounter += 4 // This should take us to the total chars in the text
					numberOfChars := pduBody[idxCounter]
					if numberOfChars > 0 {
						textMessage = string(pduBody[idxCounter+1 : idxCounter+1+int(numberOfChars)])
					}
				}

				// prepare submit_sm_resp
				msgId := strconv.Itoa(rand.Int())
				respBytes = stringBodyPDU(SUBMIT_SM_RESP, STS_OK, seqNum, msgId)

				pdu := PDUMessage{
					MsgSourceAddr:      sourceAddress,
					MsgDestinationAddr: destinationAddress,
					MsgCommand:         cmdId,
					MsgResponse:        respBytes,
					MsgID:              msgId,
					Message:            textMessage,
				}

				message <- MO{pdu}

				if registeredDlr != 0 {
					go func() {
						time.Sleep(2000 * time.Millisecond)
						now := time.Now()
						dlr, data := deliveryReceiptPDU(pdu, now, now)
						if _, err := conn.Write(data); err != nil {
							log.Printf("error sending delivery receipt to system_id[%s] due %v.", systemId, err)
							return
						} else {
							message <- DeliveryRepoort{dlr}
							log.Printf("delivery receipt for message [%s] was send to system_id[%s]", msgId, systemId)
						}
					}()
				}
			}
		case DELIVER_SM_RESP: // deliver_sm_resp
			{
				// if cmdLen > 16 {
				// 	buf := command[cmdLen-16:]
				// 	buf := make([]byte, cmdLen-16)
				// 	if _, err := io.ReadFull(conn, buf); err != nil {
				// 		log.Printf("error reading deliver_sm_resp for %s due %v. closing connection", systemId, err)
				// 		return
				// 	}
				// }
				log.Println("deliver_sm_resp from", systemId)
			}
		default:
			{
				// if cmdLen > 16 {
				// 	buf := make([]byte, cmdLen-16)
				// 	if _, err := io.ReadFull(conn, buf); err != nil {
				// 		log.Printf("error reading pdu for %s due %v. closing connection", systemId, err)
				// 		return
				// 	}
				// }
				log.Printf("unsupported pdu cmd_id(%d) from %s", cmdId, systemId)
				// generic nack packet with status "Invalid Command ID"
				respBytes = headerPDU(GENERIC_NACK, STS_INVALID_CMD, seqNum)
			}
		}

		if _, err := conn.Write(respBytes); err != nil {
			log.Printf("error sending response to system_id[%s] due %v. closing connection", systemId, err)
			return
		}
	}
}

func headerPDU(cmdId, cmdSts, seqNum uint32) []byte {
	buf := make([]byte, 16)
	binary.BigEndian.PutUint32(buf[0:], 16)
	binary.BigEndian.PutUint32(buf[4:], cmdId)
	binary.BigEndian.PutUint32(buf[8:], cmdSts)
	binary.BigEndian.PutUint32(buf[12:], seqNum)
	return buf
}

func stringBodyPDU(cmdId, cmdSts, seqNum uint32, body string) []byte {
	cmdLen := 16 + len(body) + 1 // 16 for header + body length with null terminator
	buf := make([]byte, 16)
	binary.BigEndian.PutUint32(buf[0:], uint32(cmdLen))
	binary.BigEndian.PutUint32(buf[4:], cmdId)
	binary.BigEndian.PutUint32(buf[8:], cmdSts)
	binary.BigEndian.PutUint32(buf[12:], seqNum)
	buf = append(buf, body...)
	buf = append(buf, "\x00"...)
	return buf
}

const DELIVERY_RECEIPT_FORMAT = "id:%s sub:001 dlvrd:001 submit date:%s done date:%s stat:DELIVRD err:000 Text:%s"

func deliveryReceiptPDU(message PDUMessage, submitDate, doneDate time.Time) (PDUMessage, []byte) {
	sbtDateFrmt := submitDate.Format("0601021504")
	doneDateFrmt := doneDate.Format("0601021504")
	deliveryReceipt := fmt.Sprintf(DELIVERY_RECEIPT_FORMAT, message.MessageId(), sbtDateFrmt, doneDateFrmt, message.MessageReceived())
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

	data := deliverSmPDU(message, CODING_DEFAULT, rand.Int(), tlvs, true)
	return message, data
}

func deliverSmPDU(message PDUMessage, coding byte, seqNum int, tlvs []Tlv, isDLR bool) []byte {
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
	if isDLR {
		buf.WriteString(string([]byte{4})) // esm class 4
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
