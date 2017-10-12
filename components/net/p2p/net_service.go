// Copyright (C) 2017 go-nebulas authors
//
// This file is part of the go-nebulas library.
//
// the go-nebulas library is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// the go-nebulas library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with the go-nebulas library.  If not, see <http://www.gnu.org/licenses/>.
//

package p2p

import (
	"bytes"
	"encoding/json"
	"errors"
	"hash/crc32"
	"io"
	"sync"

	"github.com/gogo/protobuf/proto"
	kbucket "github.com/libp2p/go-libp2p-kbucket"
	nnet "github.com/libp2p/go-libp2p-net"
	peer "github.com/libp2p/go-libp2p-peer"
	peerstore "github.com/libp2p/go-libp2p-peerstore"
	ma "github.com/multiformats/go-multiaddr"
	"github.com/nebulasio/go-nebulas/components/net"
	"github.com/nebulasio/go-nebulas/components/net/messages"
	"github.com/nebulasio/go-nebulas/core"
	"github.com/nebulasio/go-nebulas/core/pb"
	byteutils "github.com/nebulasio/go-nebulas/util/byteutils"
	log "github.com/sirupsen/logrus"
)

// const define constant
const (
	ProtocolID     = "/neb/1.0.0"
	HELLO          = "hello"
	OK             = "ok"
	BYE            = "bye"
	SYNCROUTE      = "syncroute"
	NEWBLOCK       = "newblock"
	SYNCROUTEREPLY = "resyncroute"
)

// MagicNumber the protocol magic number, A constant numerical or text value used to identify protocol.
var MagicNumber = []byte{0x4e, 0x45, 0x42, 0x31}

// NetService service for nebulas p2p network
type NetService struct {
	node       *Node
	quitCh     chan bool
	dispatcher *net.Dispatcher
}

// NewNetService create netService
func NewNetService(config *Config) *NetService {
	if config == nil {
		config = DefautConfig()
	}
	n, err := NewNode(config)
	if err != nil {
		log.Error("NewNetService: node create fail -> ", err)
	}
	ns := &NetService{n, make(chan bool), net.NewDispatcher()}
	return ns
}

// RegisterNetService register to Netservice
func (ns *NetService) RegisterNetService() *NetService {
	ns.node.host.SetStreamHandler(ProtocolID, ns.streamHandler)
	log.Infof("RegisterNetService: register netservice success")
	return ns
}

func (ns *NetService) streamHandler(s nnet.Stream) {
	go (func() {
		node := ns.node
	HandleMsg:
		for {
			select {
			case <-ns.quitCh:
				break HandleMsg
			default:
				pid := s.Conn().RemotePeer()
				addrs := s.Conn().RemoteMultiaddr()
				dataHeader, err := ReadUint32(s, 36)
				if err != nil {
					log.Error("streamHandler: read data header occurs error, ", err)
					node.Bye(pid, []ma.Multiaddr{addrs})
					break HandleMsg
				}

				magicNumber := dataHeader[:4]
				chainID := dataHeader[4:8]
				version := []byte{dataHeader[11]}
				msgName := dataHeader[12:24]
				dataLength := dataHeader[24:28]
				dataChecksum := dataHeader[28:32]

				index := bytes.IndexByte(msgName, 0)
				msgNameByte := msgName[0:index]
				msgNameStr := string(msgNameByte)

				log.Infof("streamHandler:handle coming msg remote addrs -> %s [msgName -> %s, magicNumber -> %s, chainID -> %d, version -> %d]", addrs, msgNameStr, string(magicNumber), byteutils.Uint32(chainID), version[0])

				if !node.verifyHeader(magicNumber, chainID, version) {
					node.Bye(pid, []ma.Multiaddr{addrs})
					break HandleMsg
				}
				data, err := ReadUint32(s, byteutils.Uint32(dataLength))
				if err != nil {
					log.Error("streamHandler: read data occurs error, ", err)
					node.Bye(pid, []ma.Multiaddr{addrs})
					break HandleMsg
				}
				dataChecksumA := crc32.ChecksumIEEE(data)
				if dataChecksumA != byteutils.Uint32(dataChecksum) {
					log.Infof("streamHandler: dataChecksumA -> %d, dataChecksum -> %d", dataChecksumA, byteutils.Uint32(dataChecksum))
					log.Error("streamHandler: data verification occurs error, dataChecksum is error, the connection will be closed.")
					break HandleMsg
				}

				switch msgNameStr {
				case HELLO:
					log.Info("streamHandler: [HELLO] handle hello message")
					okdata := []byte(OK)
					totalData := ns.buildData(okdata, OK)

					if err := Write(s, totalData); err != nil {
						log.Error("streamHandler: [HELLO] write data occurs error, ", err)
						break HandleMsg
					}
					node.stream[addrs.String()] = s
					node.conn[addrs.String()] = S_OK
					node.routeTable.Update(pid)
				case OK:
					log.Infof("streamHandler: [OK] handle ok message data -> %s", string(data))
					okstr := string(data)
					if okstr == OK {
						node.conn[addrs.String()] = S_OK
						node.stream[addrs.String()] = s
						node.routeTable.Update(pid)
					} else {
						log.Error("streamHandler: [OK] say hello get incorrect response")
						break HandleMsg
					}

				case BYE:
				case NEWBLOCK:
					log.Info("streamHandler: [NEWBLOCK] handle new block message")
					block := new(core.Block)
					pb := new(corepb.Block)
					if err := proto.Unmarshal(data, pb); err != nil {
						log.Error("streamHandler: [NEWBLOCK] handle new block msg occurs error: ", err)
						ns.quitCh <- true
					}
					if err := block.FromProto(pb); err != nil {
						log.Error("streamHandler: [NEWBLOCK] handle new block msg occurs error: ", err)
						ns.quitCh <- true
					}
					msg := messages.NewBaseMessage(msgNameStr, block)
					ns.PutMessage(msg)
				case SYNCROUTE:
					log.Info("streamHandler: [SYNCROUTE] handle sync route message")
					peers := node.routeTable.NearestPeers(kbucket.ConvertPeerID(pid), node.config.maxSyncNodes)
					var peerList []peerstore.PeerInfo
					for i := range peers {
						peerInfo := node.peerstore.PeerInfo(peers[i])
						if len(peerInfo.Addrs) == 0 {
							log.Warn("streamHandler: [SYNCROUTE] addrs is nil")
							continue
						}
						peerList = append(peerList, peerInfo)
					}
					log.Info("streamHandler: [SYNCROUTE] handle sync route request and return data -> ", peerList)

					data, err := json.Marshal(peerList)
					if err != nil {
						log.Error("streamHandler: [SYNCROUTE] handle sync route occurs error...", err)
						break HandleMsg
					}

					totalData := ns.buildData(data, SYNCROUTEREPLY)

					stream := node.stream[addrs.String()]
					if stream == nil {
						log.Error("streamHandler: [SYNCROUTE] send message occrus error, stream does not exist.")
						return
					}
					if err := Write(stream, totalData); err != nil {
						log.Error("streamHandler: [SYNCROUTE] write data occurs error, ", err)
						break HandleMsg
					}

					node.routeTable.Update(pid)

				case SYNCROUTEREPLY:
					log.Infof("streamHandler: [SYNCROUTEREPLY] handle sync route reply ")
					var sample []peerstore.PeerInfo

					json.Unmarshal(data, &sample)
					log.Infof("streamHandler: [SYNCROUTEREPLY] handle sync route reply from %s success and get response...%s", pid, sample)

					for i := range sample {
						if node.routeTable.Find(sample[i].ID) != "" || len(sample[i].Addrs) == 0 {
							log.Warnf("streamHandler: [SYNCROUTEREPLY] node %s is already exist in route table", sample[i].ID)
							continue
						}
						// Ping the peer.
						node.peerstore.AddAddr(
							sample[i].ID,
							sample[i].Addrs[0],
							peerstore.TempAddrTTL,
						)

						if err := ns.Hello(sample[i].ID); err != nil {
							log.Errorf("streamHandler: [SYNCROUTEREPLY] ping peer %s fail %s", sample[i].ID, err)
							continue
						}
						node.peerstore.AddAddr(
							sample[i].ID,
							sample[i].Addrs[0],
							peerstore.PermanentAddrTTL,
						)

						// Update the routing table.
						node.routeTable.Update(sample[i].ID)
					}

				}

			}
		}
	})()

}

func (node *Node) verifyHeader(magicNumber []byte, chainID []byte, version []byte) bool {
	if !byteutils.Equal(MagicNumber, magicNumber) {
		log.Error("verifyHeader: data verification occurs error, magic number is error, the connection will be closed.")
		return false
	}

	if node.chainID != byteutils.Uint32(chainID) {
		log.Error("verifyHeader: data verification occurs error, chainID is error, the connection will be closed.")
		return false
	}

	if !byteutils.Equal([]byte{node.version}, version) {
		log.Error("verifyHeader: data verification occurs error, version is error, the connection will be closed.")
		return false
	}
	return true
}

// Bye say bye to a peer, and close connection.
func (node *Node) Bye(pid peer.ID, addrs []ma.Multiaddr) {
	node.peerstore.SetAddrs(pid, addrs, 0)
	node.routeTable.Remove(pid)
	node.conn[addrs[0].String()] = S_NC
	// Say Bye bye!
}

// SendMsg send message to a peer
func (ns *NetService) SendMsg(msgName string, msg []byte, pid peer.ID) {
	node := ns.node
	addrs := node.peerstore.PeerInfo(pid).Addrs
	log.Infof("SendMsg: send message to addrs -> %s, msgName -> %s", addrs, msgName)
	if len(addrs) < 0 {
		log.Error("SendMsg: wrong pid addrs")
		return
	}
	data := msg
	totalData := ns.buildData(data, msgName)

	stream := node.stream[addrs[0].String()]
	if stream == nil {
		log.Error("SendMsg: send message occrus error, stream does not exist.")
		return
	}
	if err := Write(stream, totalData); err != nil {
		log.Error("SendMsg: write data occurs error, ", err)
		return
	}

}

// Hello say hello to a peer
func (ns *NetService) Hello(pid peer.ID) error {

	msgName := HELLO
	node := ns.node
	stream, err := node.host.NewStream(
		node.context,
		pid,
		ProtocolID,
	)
	addrs := node.peerstore.PeerInfo(pid).Addrs
	if err != nil {
		node.peerstore.SetAddrs(pid, addrs, 0)
		node.routeTable.Remove(pid)
		return err
	}
	if len(addrs) < 1 {
		log.Error("Hello: wrong pid addrs")
		return errors.New("wrong pid addrs")
	}

	log.Infof("Hello: say hello addrs -> %s", addrs)

	data := []byte(msgName)
	totalData := ns.buildData(data, msgName)

	if err := Write(stream, totalData); err != nil {
		log.Error("Hello: write data occurs error, ", err)
		return errors.New("Hello: write data occurs error")
	}
	ns.streamHandler(stream)
	return nil
}

// SyncRoutes sync routing table from a peer
func (ns *NetService) SyncRoutes(pid peer.ID) {
	log.Info("SyncRoutes: begin to sync route from ", pid)
	node := ns.node
	addrs := node.peerstore.PeerInfo(pid).Addrs
	if len(addrs) < 0 {
		log.Error("SyncRoutes: wrong pid addrs")
		return
	}
	data := []byte(SYNCROUTE)
	totalData := ns.buildData(data, SYNCROUTE)

	stream := node.stream[addrs[0].String()]
	if stream == nil {
		log.Error("SyncRoutes: send message occrus error, stream does not exist.")
		// return nil, errors.New("SyncRoutes: send message occrus error, stream does not exist")
		return
	}

	if err := Write(stream, totalData); err != nil {
		log.Error("SyncRoutes: write data occurs error, ", err)
		return
	}

}

// buildHeader build header information
func buildHeader(chainID uint32, msgName string, version byte, dataLength uint32, dataChecksum uint32) []byte {
	var metaHeader = make([]byte, 32)
	msgNameByte := []byte(msgName)

	copy(metaHeader[00:], MagicNumber)
	copy(metaHeader[04:], byteutils.FromUint32(chainID))
	// 64-88 Reserved field
	copy(metaHeader[11:], []byte{version})
	copy(metaHeader[12:], msgNameByte)
	copy(metaHeader[24:], byteutils.FromUint32(dataLength))
	copy(metaHeader[28:], byteutils.FromUint32(dataChecksum))

	return metaHeader
}

func (ns *NetService) buildData(data []byte, msgName string) []byte {
	node := ns.node
	dataChecksum := crc32.ChecksumIEEE(data)
	metaHeader := buildHeader(node.chainID, msgName, node.version, uint32(len(data)), dataChecksum)
	headerChecksum := crc32.ChecksumIEEE(metaHeader)
	metaHeader = append(metaHeader[:], byteutils.FromUint32(headerChecksum)...)
	totalData := append(metaHeader[:], data...)
	return totalData
}

// Start start p2p manager.
func (ns *NetService) Start() {
	// ns.startStreamHandler()
	ns.Launch()
	ns.dispatcher.Start()

}

// Stop stop p2p manager.
func (ns *NetService) Stop() {
	ns.dispatcher.Stop()
	ns.quitCh <- true
}

// Register register the subscribers.
func (ns *NetService) Register(subscribers ...*net.Subscriber) {
	ns.dispatcher.Register(subscribers...)
}

// Deregister Deregister the subscribers.
func (ns *NetService) Deregister(subscribers ...*net.Subscriber) {
	ns.dispatcher.Deregister(subscribers...)
}

// PutMessage put message to dispatcher.
func (ns *NetService) PutMessage(msg net.Message) {
	ns.dispatcher.PutMessage(msg)
}

// BroadcastBlock broadcast block message
func (ns *NetService) BroadcastBlock(block interface{}) {
	//TODO: broadcast block via underlying network lib to whole network.
	ns.Broadcast(block)
}

// Launch start netService
func (ns *NetService) Launch() error {

	node := ns.node
	log.Infof("Launch: node info {id -> %s, address -> %s}", node.id, node.host.Addrs())
	if node.running {
		return errors.New("Launch: node already running")
	}
	node.running = true
	log.Info("Launch: node start to join p2p network...")

	ns.RegisterNetService()

	var wg sync.WaitGroup
	for _, bootNode := range node.config.BootNodes {
		wg.Add(1)
		go func(bootNode ma.Multiaddr) {
			defer wg.Done()
			err := ns.SayHello(bootNode)
			if err != nil {
				log.Error("Launch: can not say hello to trusted node.", bootNode, err)
			}

		}(bootNode)
	}
	wg.Wait()

	go func() {
		ns.Discovery(node.context)
	}()

	log.Infof("Launch: node start and join to p2p network success and listening for connections on port %d... ", node.config.Port)

	return nil
}

func Write(writer io.Writer, data []byte) error {
	result := make(chan error, 1)
	go func(writer io.Writer, data []byte) {
		_, err := writer.Write(data)
		result <- err
	}(writer, data)
	err := <-result
	return err
}

func ReadUint32(reader io.Reader, n uint32) ([]byte, error) {
	data := make([]byte, n)
	result := make(chan error, 1)
	go func(reader io.Reader) {
		_, err := io.ReadFull(reader, data)
		result <- err
	}(reader)
	err := <-result
	return data, err
}
