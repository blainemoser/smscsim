package smsc

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math/rand"
	"net"
	"strconv"
	"time"
)

const (
	DEFAULT_SYSTEM_ID = "anonymous"
)

type (
	handler interface {
		handle() ([]byte, error)
	}

	command struct {
		metaData        *metaData
		connection      net.Conn
		command         []byte
		pduHeaderBuffer []byte
		pduBodyBuffer   []byte
		cstrings        map[int]string
		commandLength   uint32
		commandId       uint32
		commandStatus   uint32
		sequenceNumber  uint32
	}

	bind             struct{ *command }
	unbind           struct{ *command }
	enquire          struct{ *command }
	shortMessage     struct{ *command }
	deliveryResponse struct{ *command }
	unrecognised     struct{ *command }
)

func NewCommand(meta *metaData, connection net.Conn) (c *command, err error) {
	buffer := make([]byte, 2048) // this is overkill, but it's safe; we'll truncate it. Since it's not a phone we can be a little liberal
	if _, err = connection.Read(buffer); err != nil && err != io.EOF {
		return
	}
	commandBytes, headerBuffer, bodyBuffer := getBuffers(buffer)
	cstrings := extractCStrings(bodyBuffer)
	c = &command{
		metaData:        meta,
		command:         commandBytes,
		connection:      connection,
		pduHeaderBuffer: headerBuffer,
		pduBodyBuffer:   bodyBuffer,
		cstrings:        cstrings,
		commandLength:   binary.BigEndian.Uint32(headerBuffer[0:]),
		commandId:       binary.BigEndian.Uint32(headerBuffer[4:]),
		commandStatus:   binary.BigEndian.Uint32(headerBuffer[8:]),
		sequenceNumber:  binary.BigEndian.Uint32(headerBuffer[12:]),
	}
	return
}

func getBuffers(buffer []byte) (command []byte, header []byte, body []byte) {
	command = bytes.TrimRight(buffer, "\x00")
	header = buffer[:16]
	if len(buffer) >= 16 {
		body = buffer[16:]
	} else {
		body = buffer
	}
	return
}

func (c *command) instruction() handler {
	switch c.commandId {
	case BIND_RECEIVER, BIND_TRANSMITTER, BIND_TRANSCEIVER:
		return &bind{c}
	case UNBIND:
		return &unbind{c}
	case ENQUIRE_LINK:
		return &enquire{c}
	case SUBMIT_SM:
		return &shortMessage{c}
	case DELIVER_SM_RESP:
		return &deliveryResponse{c}
	default:
		return &unrecognised{c}
	}
}

func (b *bind) handle() ([]byte, error) {
	b.metaData.Log <- LogMessage{Level: "info", Message: "Handling Bind"}
	if len(b.cstrings) < 2 {
		return nil, fmt.Errorf("closing connection due %v\n", fmt.Errorf("command not found"))
	}
	b.metaData.findSystemIdAndPassword(b.cstrings)
	respCmdId := 2147483648 + b.commandId // hack to calc resp cmd id
	var respBytes []byte
	if b.metaData.bound {
		respBytes = headerPDU(respCmdId, STS_ALREADY_BOUND, b.sequenceNumber)
		b.metaData.Log <- LogMessage{Level: "info", Message: fmt.Sprintf("[%s] already has bound session", b.metaData.systemId)}
	} else {
		receiveMo := b.commandId == BIND_RECEIVER || b.commandId == BIND_TRANSCEIVER
		b.metaData.smsc.Sessions[b.metaData.sessionId] = Session{b.metaData.systemId, b.connection, receiveMo}
		respBytes = stringBodyPDU(respCmdId, STS_OK, b.sequenceNumber, "smscsim")
		b.metaData.setBound(true)
		b.metaData.setReceiver(b.commandId == BIND_RECEIVER)
	}
	return respBytes, nil
}

func (u *unbind) handle() ([]byte, error) {
	u.metaData.Log <- LogMessage{Level: "info", Message: fmt.Sprintf("unbind request from system_id[%s]", u.metaData.systemId)}
	respBytes := headerPDU(UNBIND_RESP, STS_OK, u.sequenceNumber)
	u.metaData.setBound(false)
	u.metaData.setSysId(DEFAULT_SYSTEM_ID, "")
	return respBytes, nil
}

func (e *enquire) handle() ([]byte, error) {
	e.metaData.Log <- LogMessage{Level: "info", Message: fmt.Sprintf("enquire_link from system_id[%s]", e.metaData.systemId)}
	respBytes := headerPDU(ENQUIRE_LINK_RESP, STS_OK, e.sequenceNumber)
	return respBytes, nil
}

func (s *shortMessage) handle() ([]byte, error) {
	var respBytes []byte
	if s.metaData.receiver {
		respBytes = headerPDU(SUBMIT_SM_RESP, STS_INV_BIND_STS, s.sequenceNumber)
		return nil, fmt.Errorf("error handling submit_sm from system_id[%s]. session with bind type RECEIVER cannot send requests", s.metaData.systemId)
	}

	var sourceAddress string
	var destinationAddress string
	var srvType string
	var scheduleEnd string
	var validityEnd string
	var ok bool

	// https://smpp.org/
	idxCounter := 0

	// service_type Var.
	// Max 6
	// C-Octet
	// String
	// Indicates the type of service associated with the
	// message.
	// Where not required this should be set to a single
	// NULL byte.
	if srvType, ok = s.cstrings[idxCounter]; !ok {
		if bytes.Index(s.pduBodyBuffer[idxCounter:], nullTerm) == -1 {
			respBytes = headerPDU(GENERIC_NACK, STS_INVALID_CMD, s.sequenceNumber)
			return respBytes, nil
		}
	}
	idxCounter += len(srvType) + 1 // NOW AT Source TON we're not using the service type for now, but it should be in the c-strings if needed
	idxCounter += 2                // NOW AT Source Address; skip npi. if needed, these should be at the following 2 positions after the last service character

	// srcAddrEndIdx := bytes.Index(pduBody[idxCounter:], nullTerm)
	if sourceAddress, ok = s.cstrings[idxCounter]; !ok {
		respBytes = headerPDU(GENERIC_NACK, STS_INVALID_CMD, s.sequenceNumber)
		return respBytes, nil
	}
	idxCounter += len(sourceAddress) + 1 // move up one index position after source address; NOW AT Destination TON
	idxCounter = idxCounter + 2          // skip dest ton and npi. if needed, they will be at the two positions after the last source address character

	// destAddrEndIdx := bytes.Index(pduBody[idxCounter:], nullTerm)
	if destinationAddress, ok = s.cstrings[idxCounter]; !ok {
		respBytes = headerPDU(GENERIC_NACK, STS_INVALID_CMD, s.sequenceNumber)
		return respBytes, nil
	}
	idxCounter += len(destinationAddress) + 1 // move up one index after the destination address
	esmClass := s.pduBodyBuffer[idxCounter]

	// For UDH, the first seven characters of the message body represent the headers.
	// we can parse that when it comes to the message body
	idxCounter = idxCounter + 3 // skip esm_class, protocol_id, priority_flag

	if scheduleEnd, ok = s.cstrings[idxCounter]; !ok {
		if bytes.Index(s.pduBodyBuffer[idxCounter:], nullTerm) == -1 {
			respBytes = headerPDU(GENERIC_NACK, STS_INVALID_CMD, s.sequenceNumber)
			return respBytes, nil
		}
	}
	idxCounter += len(scheduleEnd) + 1 // one char after the schedule end c-string; next is validity period

	if validityEnd, ok = s.cstrings[idxCounter]; !ok {
		if bytes.Index(s.pduBodyBuffer[idxCounter:], nullTerm) == -1 {
			respBytes = headerPDU(GENERIC_NACK, STS_INVALID_CMD, s.sequenceNumber)
			return respBytes, nil
		}
	}
	idxCounter += len(validityEnd) + 1           // counter should now be on the register DLR flag
	registeredDlr := s.pduBodyBuffer[idxCounter] // registered_delivery is next field after the validity_period

	// we're not too interested in the fields between the "register DLR" flag and the actual text of the message.
	// Therefore, if we have the space, we'll check for text from the remaining cstring (it would be the last
	// possible one) and use that
	var textMessage string
	if len(s.pduBodyBuffer) >= idxCounter+4 {
		idxCounter += 4 // This should take us to the total chars in the text
		numberOfChars := s.pduBodyBuffer[idxCounter]
		if numberOfChars > 0 {
			textMessage = string(s.pduBodyBuffer[idxCounter+1 : idxCounter+1+int(numberOfChars)])
		}
	}

	// prepare submit_sm_resp
	msgId := strconv.Itoa(rand.Int())
	respBytes = stringBodyPDU(SUBMIT_SM_RESP, STS_OK, s.sequenceNumber, msgId)
	messageReady := true
	if esmClass == 64 {
		part, err := multipartMessages.handle([]byte(textMessage), sourceAddress, destinationAddress)
		if err != nil {
			return nil, err
		}
		if messageReady = part.ready(); messageReady {
			textMessage = part.getMessages()
			multipartMessages.clear(part.id)
		}
	}
	fmt.Println(messageReady)

	pdu := PDUMessage{
		MsgSourceAddr:      sourceAddress,
		MsgDestinationAddr: destinationAddress,
		MsgCommand:         s.commandId,
		MsgResponse:        respBytes,
		MsgID:              msgId,
		Message:            textMessage,
	}

	if messageReady {
		s.metaData.message <- MO{pdu}
	}

	if registeredDlr != 0 {
		go func() {
			time.Sleep(2000 * time.Millisecond)
			now := time.Now()
			fmt.Println("delivering message", pdu.MessageId())
			_, data := deliveryReceiptPDU(pdu, now, now)
			if _, err := s.connection.Write(data); err != nil {
				s.metaData.Log <- LogMessage{Message: fmt.Sprintf("error sending delivery receipt to system_id[%s] due %v.", s.metaData.systemId, err), Level: "error"}
				return
			}
			logMsg := fmt.Sprintf("delivery receipt for message [%s] was send to system_id[%s]", msgId, s.metaData.systemId)
			s.metaData.Log <- LogMessage{Message: logMsg, Level: "info"}
		}()
	}
	return respBytes, nil
}

func (d *deliveryResponse) handle() ([]byte, error) {
	dlrID := binary.BigEndian.Uint32(d.command.command[12:])
	// pdumsg := PDUMessage{
	// }
	pdumsg := pendingDeliveries.pop(dlrID)
	if pdumsg != nil {
		pdumsg.Message = string(d.command.command)
		d.metaData.message <- DeliveryReport{*pdumsg}
	}
	d.metaData.Log <- LogMessage{Level: "info", Message: fmt.Sprintf("deliver_sm_resp from '%s'", d.metaData.systemId)}
	return nil, nil
}

func (u *unrecognised) handle() ([]byte, error) {
	u.metaData.Log <- LogMessage{Level: "warning", Message: fmt.Sprintf("unsupported pdu cmd_id(%d) from %s", u.commandId, u.metaData.systemId)}
	// generic nack packet with status "Invalid Command ID"
	respBytes := headerPDU(GENERIC_NACK, STS_INVALID_CMD, u.sequenceNumber)
	return respBytes, nil
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
