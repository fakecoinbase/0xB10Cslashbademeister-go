package zmqsubscriber

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"syscall"
	"time"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/txscript"

	"github.com/btcsuite/btcd/wire"

	"github.com/0xb10c/bademeister-go/src/types"

	"github.com/pebbe/zmq4"
)

// ZMQSubscriber represents a ZMQ subscriber for the Bitcoin Core ZMQ interface
type ZMQSubscriber struct {
	IncomingTx     chan types.Transaction
	IncomingBlocks chan types.Block
	topics         []string
	socket         *zmq4.Socket
	cancel         bool
}

const topicRawBlock = "rawblock"
const topicRawTxWithFee = "rawtxwithfee"

// NewZMQSubscriber creates and returns a new ZMQSubscriber,
// which subscribes and connect to a Bitcoin Core ZMQ interface.
func NewZMQSubscriber(host string, port string) (*ZMQSubscriber, error) {
	socket, err := zmq4.NewSocket(zmq4.SUB)
	if err != nil {
		return nil, err
	}

	topics := []string{topicRawTxWithFee, topicRawBlock}
	for _, topic := range topics {
		err := socket.SetSubscribe(topic)
		if err != nil {
			return nil, err
		}
	}

	connectionString := "tcp://" + host + ":" + port
	if err = socket.Connect(connectionString); err != nil {
		return nil, fmt.Errorf("could not connect ZMQ subscriber to '%s': %s", connectionString, err)
	}

	log.Printf("ZMQ subscriber successfully connected to %s", connectionString)

	incomingTx := make(chan types.Transaction)
	incomingBlocks := make(chan types.Block)

	return &ZMQSubscriber{
		topics:         topics,
		IncomingTx:     incomingTx,
		IncomingBlocks: incomingBlocks,
		socket:         socket,
		cancel:         false,
	}, nil
}

// Run starts receiving new ZMQ messages. These messages are parsed according to
// their topic and passed as native data types into the corresponding channels
// (`IncomingTx` or `IncomingBlocks`). Run returns an error if an error occurs
// while parsing. On normal stops with `Stop()` `nil` is returned.
func (z *ZMQSubscriber) Run() error {
	defer func() {
		if err := z.socket.Close(); err != nil {
			log.Printf("ZMQ subscriber socket closed with error (ignored): %s\n", err)
		}
	}()

	parseErrors := make(chan error)

	if err := z.socket.SetRcvtimeo(time.Second); err != nil {
		return fmt.Errorf("could not set a receive timeout: %s", err)
	}

	// FIXME(#11):
	// Workaround for a zmq crash when Close() is called from a different
	// goroutine. Instead of permanently blocking on Recv(), a timeout is set and
	// we check for `z.cancel`.
	for !z.cancel {
		select {
		case err := <-parseErrors:
			return err
		default:
		}

		msg, err := z.socket.RecvMessageBytes(0)
		if err != nil {
			if err == zmq4.Errno(syscall.EAGAIN) {
				log.Println("No ZMQ message received in the last second.")
				continue
			} else if err == zmq4.Errno(syscall.EINTR) {
				continue
			}
			return fmt.Errorf("could not receive ZMQ message: %s", err)
		}

		topic, payload := string(msg[0]), msg[1:]
		log.Printf("ZMQ subscriber received topic %s", topic)

		// received messages are processed asynchronously so that the queue does not
		// stall while parsing
		go func() {
			if err := z.processMessage(topic, payload); err != nil {
				parseErrors <- err
			}
		}()
	}

	return nil
}

func (z *ZMQSubscriber) processMessage(topic string, payload [][]byte) error {
	// TODO: use GetTime() and allow other time sources (eg NTP-corrected)
	firstSeen := time.Now().UTC()

	switch topic {
	case topicRawTxWithFee:
		tx, err := parseTransaction(firstSeen, payload)
		if err != nil {
			return err
		}
		z.IncomingTx <- *tx
	case topicRawBlock:
		block, err := parseBlock(firstSeen, payload)
		if err != nil {
			return err
		}
		z.IncomingBlocks <- *block
	default:
		return fmt.Errorf("unknown topic %s", topic)
	}

	return nil
}

// Stop sets the cancel flag to true. The ZMQSubscriber is stopped after it
// finishes receiving a message or reaches the timeout.
func (z *ZMQSubscriber) Stop() {
	z.cancel = true
}

func parseTransaction(firstSeen time.Time, payload [][]byte) (*types.Transaction, error) {
	if len(payload) != 2 {
		return nil, fmt.Errorf("unexpected payload length: expected len(tx hash, sequence) == 2 but got len(payload) == %d", len(payload))
	}

	// payload[1] contains a 16bit LE sequence number provided by Bitcoin Core,
	// which is not used here, but noted for completeness.
	// rawtxwithfee is the rawtx by Bitcoin Core concatinated with the 8 byte LE
	// transaction fee. This is a patch from the branch
	// https://github.com/0xB10C/bitcoin/tree/2019-10-rawtxwithfee-zmq-publisher
	rawtxwithfee, _ := payload[0], payload[1]

	length := len(rawtxwithfee)
	if length <= 8 {
		return nil, errors.New("unexpected rawtxwithfee length")
	}
	rawtx, feeBytes := rawtxwithfee[:length-8], rawtxwithfee[length-8:]

	wireTx := wire.NewMsgTx(wire.TxVersion)
	if err := wireTx.Deserialize(bytes.NewReader(rawtx)); err != nil {
		return nil, fmt.Errorf("could not deserialize the rawtx as wire.MsgTx: %s", err)
	}

	txid := types.NewHashFromArray(wireTx.TxHash())

	fee := binary.LittleEndian.Uint64(feeBytes)
	weight := wireTx.SerializeSizeStripped()*3 + wireTx.SerializeSize()

	return &types.Transaction{
		FirstSeen: firstSeen,
		TxID:      txid,
		Fee:       fee,
		Weight:    weight,
	}, nil
}

func parseBlock(firstSeen time.Time, msg [][]byte) (*types.Block, error) {
	rawblock, ctr := msg[0], msg[1]
	_ = ctr

	reader := bytes.NewReader(rawblock)

	var wireBlock wire.MsgBlock
	err := wireBlock.BtcDecode(reader, 0, wire.LatestEncoding)
	if err != nil {
		return nil, fmt.Errorf("error during BtcDecode: %s", err)
	}

	// https://bitcoin.org/en/developer-reference#coinbase
	parseHeight := func(txin wire.TxIn) (int, error) {
		data, err := txscript.PushedData(txin.SignatureScript)
		if err != nil {
			return -1, err
		}
		if len(data) != 2 {
			return -1, fmt.Errorf("unexpected count %d", len(data))
		}
		heightLE := data[0][:]
		for len(heightLE) < 4 {
			heightLE = append(heightLE, 0)
		}
		height := binary.LittleEndian.Uint32(heightLE)
		return int(height), nil
	}

	height := -1
	txHashes := []types.Hash32{}
	for _, t := range wireBlock.Transactions {
		if blockchain.IsCoinBaseTx(t) {
			height, err = parseHeight(*t.TxIn[0])
			if err != nil {
				return nil, fmt.Errorf("error parsing coinbase: %s", err)
			}
		}
		txHashes = append(txHashes, types.NewHashFromArray(t.TxHash()))
	}

	if height < 0 {
		return nil, fmt.Errorf("height not found")
	}

	// FIXME: the default zmq rawblock only provides the current best block.
	//        In a reorg, we will not be able to find the parent of a new best block.
	isBest := true

	return &types.Block{
		FirstSeen:   firstSeen,
		EncodedTime: wireBlock.Header.Timestamp,
		Hash:        types.NewHashFromArray(wireBlock.BlockHash()),
		Parent:      types.NewHashFromArray(wireBlock.Header.PrevBlock),
		TxIDs:       txHashes,
		Height:      uint32(height),
		IsBest:      isBest,
	}, nil
}
