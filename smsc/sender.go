package smsc

import (
	"fmt"
	"math/rand"

	"github.com/fiorix/go-smpp/smpp/pdu/pdutext"
)

var (
	maxCharLen uint = 133
)

func SendLongMessageParts(msg PDUMessage, session *Session) error {
	countParts := int((len(msg.MsgResponse)-1)/int(maxCharLen)) + 1
	rn := uint16(intn(0xFFFF))

	text := msg.MsgResponse
	fmt.Println(countParts, len(text), text)

	// Special thanks to fiorix
	UDHHeader := make([]byte, 7)
	UDHHeader[0] = 0x06              // length of user data header
	UDHHeader[1] = 0x08              // information element identifier, CSMS 16 bit reference number
	UDHHeader[2] = 0x04              // length of remaining header
	UDHHeader[3] = uint8(rn >> 8)    // most significant byte of the reference number
	UDHHeader[4] = uint8(rn)         // least significant byte of the reference number
	UDHHeader[5] = uint8(countParts) // total number of message parts
	for i := 0; i < countParts; i++ {
		var tlvs []Tlv
		UDHHeader[6] = uint8(i + 1) // current message part
		dlrId := rand.Uint32()
		var messageText []byte
		if i != countParts-1 {
			messageText = pdutext.Raw(append(UDHHeader, text[i*int(maxCharLen):(i+1)*int(maxCharLen)]...))
		} else {
			messageText = pdutext.Raw(append(UDHHeader, text[i*int(maxCharLen):]...))
			pendingDeliveries.insert(dlrId, msg)
		}
		msg.MsgResponse = messageText
		fmt.Println("sending long message part", dlrId)
		data := deliverSmPDU(msg, CODING_UCS2, dlrId, tlvs, MULTIPART)
		if _, err := session.Conn.Write(data); err != nil {
			return err
		}
	}
	return nil
}

func intn(n int) int {
	if n <= 0 {
		panic("invalid argument to Intn")
	}
	if n <= 1<<31-1 {
		return int(rand.Int31n(int32(n)))
	}
	return int(rand.Int63n(int64(n)))
}
