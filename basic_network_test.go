package basicnet

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/ericxtang/m3u8"

	peerstore "gx/ipfs/QmPgDWmTmuzvP7QE5zwo1TmjbJme9pmZHNujB2453jkCTr/go-libp2p-peerstore"
	peer "gx/ipfs/QmXYjuNuxVzXKJCfWasQk1RqkhVLDM9jtUKhqc2WPQmFSB/go-libp2p-peer"
	host "gx/ipfs/QmZy7c24mmkEHpNJndwgsEE3wcVxHd8yB969yTnAJFVw7f/go-libp2p-host"
	crypto "gx/ipfs/QmaPbCnUMBohSGo3KnxEa2bHqyJVVeEEcwtqJAYxerieBo/go-libp2p-crypto"
	net "gx/ipfs/QmahYsGWry85Y7WUe2SX5G4JkH2zifEQAUtJVLZ24aC9DF/go-libp2p-net"

	"github.com/golang/glog"
	"github.com/livepeer/go-livepeer/types"
)

func setupNodes() (*BasicVideoNetwork, *BasicVideoNetwork) {
	priv1, pub1, _ := crypto.GenerateKeyPair(crypto.RSA, 2048)
	no1, _ := NewNode(15000, priv1, pub1)
	n1, _ := NewBasicVideoNetwork(no1)

	priv2, pub2, _ := crypto.GenerateKeyPair(crypto.RSA, 2048)
	no2, _ := NewNode(15001, priv2, pub2)
	n2, _ := NewBasicVideoNetwork(no2)

	return n1, n2
}

func connectHosts(h1, h2 host.Host) {
	h1.Peerstore().AddAddrs(h2.ID(), h2.Addrs(), peerstore.PermanentAddrTTL)
	h2.Peerstore().AddAddrs(h1.ID(), h1.Addrs(), peerstore.PermanentAddrTTL)
	err := h1.Connect(context.Background(), peerstore.PeerInfo{ID: h2.ID()})
	if err != nil {
		glog.Errorf("Cannot connect h1 with h2: %v", err)
	}
	err = h2.Connect(context.Background(), peerstore.PeerInfo{ID: h1.ID()})
	if err != nil {
		glog.Errorf("Cannot connect h2 with h1: %v", err)
	}

	// Connection might not be formed right away under high load.  See https://github.com/libp2p/go-libp2p-kad-dht/blob/master/dht_test.go (func connect)
	time.Sleep(time.Millisecond * 500)
}

func TestSendBroadcast(t *testing.T) {
	glog.Infof("\n\nTesting Broadcast Stream...")
	n1, _ := setupNodes()
	//n2 is simple node so we can register our own handler and inspect the incoming messages
	n2, _ := simpleNodes(15002, 15003)
	connectHosts(n1.NetworkNode.PeerHost, n2.PeerHost)

	var strmData StreamDataMsg
	var finishStrm FinishStreamMsg
	//Set up handler
	n2.PeerHost.SetStreamHandler(Protocol, func(s net.Stream) {
		ws := NewBasicStream(s)
		var msg Msg
		err := ws.ReceiveMessage(&msg)
		if err != nil {
			glog.Errorf("Got error decoding msg: %v", err)
			return
		}
		switch msg.Data.(type) {
		case StreamDataMsg:
			strmData, _ = msg.Data.(StreamDataMsg)
		case FinishStreamMsg:
			finishStrm, _ = msg.Data.(FinishStreamMsg)

		}
	})

	b1tmp, _ := n1.GetBroadcaster("strm")
	b1, _ := b1tmp.(*BasicBroadcaster)
	//Create a new stream, this is the communication channel
	ns1, err := n1.NetworkNode.PeerHost.NewStream(context.Background(), n2.Identity, Protocol)
	if err != nil {
		t.Errorf("Cannot create stream: %v", err)
	}

	//Add the stream as a listner in the broadcaster so it can be used to send out the message
	b1.listeners[peer.IDHexEncode(ns1.Conn().RemotePeer())] = NewBasicStream(ns1)

	if b1.working != false {
		t.Errorf("broadcaster shouldn't be working yet")
	}

	//Send out the message, this should kick off the broadcaster worker
	b1.Broadcast(0, []byte("test bytes"))

	if b1.working == false {
		t.Errorf("broadcaster shouldn be working yet")
	}

	//Wait until the result var is assigned
	start := time.Now()
	for time.Since(start) < 1*time.Second {
		if strmData.StrmID == "" {
			time.Sleep(time.Millisecond * 500)
		} else {
			break
		}
	}

	if strmData.StrmID == "" {
		t.Errorf("Never got the message")
	}

	if strmData.SeqNo != 0 {
		t.Errorf("Expecting seqno to be 0, but got %v", strmData.SeqNo)
	}

	if strmData.StrmID != "strm" {
		t.Errorf("Expecting strmID to be 'strm', but got %v", strmData.StrmID)
	}

	if string(strmData.Data) != "test bytes" {
		t.Errorf("Expecting data to be 'test bytes', but got %v", strmData.Data)
	}
}

func TestHandleBroadcast(t *testing.T) {
	glog.Infof("\n\nTesting Handle Broadcast...")
	n1, _ := setupNodes()
	n2, _ := simpleNodes(15002, 15003)
	connectHosts(n1.NetworkNode.PeerHost, n2.PeerHost)

	var cancelMsg CancelSubMsg
	//Set up n2 handler so n1 can create a stream to it.
	n2.PeerHost.SetStreamHandler(Protocol, func(s net.Stream) {
		ws := NewBasicStream(s)
		var msg Msg
		err := ws.ReceiveMessage(&msg)
		if err != nil {
			glog.Errorf("Got error decoding msg: %v", err)
			return
		}
		cancelMsg, _ = msg.Data.(CancelSubMsg)
	})

	err := handleStreamData(n1, StreamDataMsg{SeqNo: 100, StrmID: "strmID", Data: []byte("hello")})
	if err != ErrProtocol {
		t.Errorf("Expecting error because no subscriber has been assigned")
	}

	s1tmp, _ := n1.GetSubscriber("strmID")
	s1, _ := s1tmp.(*BasicSubscriber)
	//Set up the subscriber to handle the streamData
	ctxW, cancel := context.WithCancel(context.Background())
	s1.cancelWorker = cancel
	s1.working = true
	s1.networkStream = n1.NetworkNode.GetStream(n2.Identity)
	var seqNoResult uint64
	var dataResult []byte
	s1.startWorker(ctxW, n2.Identity, s1.networkStream, func(seqNo uint64, data []byte, eof bool) {
		seqNoResult = seqNo
		dataResult = data
	})
	n1.subscribers["strmID"] = s1
	err = handleStreamData(n1, StreamDataMsg{SeqNo: 100, StrmID: "strmID", Data: []byte("hello")})
	if err != nil {
		t.Errorf("handleStreamData error: %v", err)
	}

	//Wait until the result vars are assigned
	start := time.Now()
	for time.Since(start) < 1*time.Second {
		if seqNoResult == 0 {
			time.Sleep(time.Millisecond * 100)
		} else {
			break
		}
	}

	if seqNoResult != 100 {
		t.Errorf("Expecting seqNo to be 100, but got: %v", seqNoResult)
	}

	if string(dataResult) != "hello" {
		t.Errorf("Expecting data to be 'hello', but got: %v", dataResult)
	}

	//Test cancellation
	s1.cancelWorker()
	//Wait for cancelMsg to be assigned
	start = time.Now()
	for time.Since(start) < 1*time.Second {
		if cancelMsg.StrmID == "" {
			time.Sleep(time.Millisecond * 100)
		} else {
			break
		}
	}
	if s1.working {
		t.Errorf("Subscriber worker shouldn't be working anymore")
	}
	if s1.networkStream != nil {
		t.Errorf("networkStream should be nil, but got: %v", s1.networkStream)
	}
	if cancelMsg.StrmID != "strmID" {
		t.Errorf("Expecting cancelMsg.StrmID to be 'strmID' (cancelMsg to be sent because of cancelWorker()), but got %v", cancelMsg.StrmID)
	}
}

func TestSendSubscribe(t *testing.T) {
	glog.Infof("\n\nTesting Subscriber...")
	n1, _ := setupNodes()
	n2, _ := simpleNodes(15002, 15003)
	connectHosts(n1.NetworkNode.PeerHost, n2.PeerHost)

	var subReq SubReqMsg
	var cancelMsg CancelSubMsg
	//Set up handler for simple node (get a subReqMsg, write a streamDataMsg back)
	n2.PeerHost.SetStreamHandler(Protocol, func(s net.Stream) {
		ws := NewBasicStream(s)
		for {
			var msg Msg
			err := ws.ReceiveMessage(&msg)
			if err != nil {
				glog.Errorf("Got error decoding msg: %v", err)
				return
			}
			switch msg.Data.(type) {
			case SubReqMsg:
				subReq, _ = msg.Data.(SubReqMsg)
				// glog.Infof("Got SubReq %v", subReq)

				for i := 0; i < 10; i++ {
					//TODO: Sleep here is needed, because we can't handle the messages fast enough.
					//I think we need to re-organize our code to kick off goroutines / workers instead of handling everything in a for loop.
					time.Sleep(time.Millisecond * 100)
					err = ws.SendMessage(StreamDataID, StreamDataMsg{SeqNo: uint64(i), StrmID: subReq.StrmID, Data: []byte("test data")})
				}
			case CancelSubMsg:
				cancelMsg, _ = msg.Data.(CancelSubMsg)
				glog.Infof("Got CancelMsg %v", cancelMsg)
			}
		}
	})

	s1tmp, _ := n1.GetSubscriber("strmID")
	s1, _ := s1tmp.(*BasicSubscriber)
	result := make(map[uint64][]byte)
	lock := &sync.Mutex{}
	s1.Subscribe(context.Background(), func(seqNo uint64, data []byte, eof bool) {
		glog.Infof("Got response: %v, %v", seqNo, data)
		lock.Lock()
		result[seqNo] = data
		lock.Unlock()
	})

	if s1.cancelWorker == nil {
		t.Errorf("Cancel function should be assigned")
	}

	//Wait until the result var is assigned
	start := time.Now()
	for time.Since(start) < 1*time.Second {
		if subReq.StrmID == "" {
			time.Sleep(time.Millisecond * 100)
		} else {
			break
		}
	}

	if subReq.StrmID != "strmID" {
		t.Errorf("Expecting subReq.StrmID to be 'strmID', but got %v", subReq.StrmID)
	}

	if !s1.working {
		t.Errorf("Subscriber should be working")
	}

	time.Sleep(time.Millisecond * 1500)

	if len(result) != 10 {
		t.Errorf("Expecting length of result to be 10, but got %v: %v", len(result), result)
	}

	for _, d := range result {
		if string(d) != "test data" {
			t.Errorf("Expecting data to be 'test data', but got %v", d)
		}
	}

	//Call cancel
	s1.cancelWorker()
	start = time.Now()
	for time.Since(start) < 2*time.Second {
		if cancelMsg.StrmID == "" {
			time.Sleep(time.Millisecond * 100)
		} else {
			break
		}
	}

	if cancelMsg.StrmID != "strmID" {
		t.Errorf("Expecting to get cancelMsg with StrmID: 'strmID', but got %v", cancelMsg.StrmID)
	}
	if s1.working {
		t.Errorf("subscriber shouldn't be working after 'cancel' is called")
	}

}

func TestHandleSubscribe(t *testing.T) {
	glog.Infof("\n\nTesting Handle Broadcast...")
	n1, _ := setupNodes()
	n2, _ := simpleNodes(15002, 15003)
	connectHosts(n1.NetworkNode.PeerHost, n2.PeerHost)

	n2.PeerHost.SetStreamHandler(Protocol, func(s net.Stream) {
		ws := NewBasicStream(s)
		var msg Msg
		err := ws.ReceiveMessage(&msg)
		if err != nil {
			glog.Errorf("Got error decoding msg: %v", err)
			return
		}
		glog.Infof("Got msg: %v", msg)
	})

	//Test when the broadcaster is local
	b1tmp, _ := n1.GetBroadcaster("strmID")
	b1, _ := b1tmp.(*BasicBroadcaster)
	n1.broadcasters["strmID"] = b1
	ws := n1.NetworkNode.GetStream(n2.Identity)
	handleSubReq(n1, SubReqMsg{StrmID: "strmID"}, ws)

	l := b1.listeners[peer.IDHexEncode(n2.Identity)]
	if l == nil || reflect.TypeOf(l) != reflect.TypeOf(&BasicStream{}) {
		t.Errorf("Expecting l to be assigned a BasicStream, but got :%v", reflect.TypeOf(l))
	}
	delete(n1.broadcasters, "strmID")

	//Test when the broadcaster is remote, and there is already a relayer.
	r1 := n1.NewRelayer("strmID")
	if n1.relayers["strmID"] != r1 {
		t.Errorf("Should have assigned relayer")
	}
	handleSubReq(n1, SubReqMsg{StrmID: "strmID"}, ws)
	pid := peer.IDHexEncode(ws.Stream.Conn().RemotePeer())
	if r1.listeners[pid] != ws {
		t.Errorf("Should have assigned listener to relayer")
	}
	delete(n1.relayers, "strmID")

	//Test when the broadcaster is remote, and there isn't a relayer yet.
	//TODO: This is hard to test because of the dependency to kad.IpfsDht.  We can get around it by creating an interface called "NetworkRouting"
	// handleSubReq(n1, SubReqMsg{StrmID: "strmID"}, ws)

}

func simpleRelayHandler(ws *BasicStream, t *testing.T) Msg {
	var msg Msg
	err := ws.ReceiveMessage(&msg)
	if err != nil {
		glog.Errorf("Got error decoding msg: %v", err)
		return Msg{}
	}
	return msg
}
func TestRelaying(t *testing.T) {
	n1, n2 := setupNodes()
	n3, _ := simpleNodes(15002, 15003)
	connectHosts(n1.NetworkNode.PeerHost, n2.NetworkNode.PeerHost)
	connectHosts(n2.NetworkNode.PeerHost, n3.PeerHost)

	strmID := peer.IDHexEncode(n1.NetworkNode.Identity) + "strmID"
	b1, _ := n1.GetBroadcaster(strmID)
	go n1.SetupProtocol()
	go n2.SetupProtocol()

	s3 := n3.GetStream(n2.NetworkNode.Identity)
	s3.SendMessage(SubReqID, SubReqMsg{StrmID: strmID})

	var strmDataResult StreamDataMsg
	var finishResult FinishStreamMsg
	var ok bool
	go func() {
		for {
			msg := simpleRelayHandler(s3, t)

			glog.Infof("Got msg: %v", msg)
			switch msg.Data.(type) {
			case StreamDataMsg:
				strmDataResult, ok = msg.Data.(StreamDataMsg)
				if !ok {
					t.Errorf("Expecting stream data to come back")
				}
			case FinishStreamMsg:
				finishResult, ok = msg.Data.(FinishStreamMsg)
				if !ok {
					t.Errorf("Expecting finish stream to come back")
				}
			}
		}
	}()

	time.Sleep(time.Second * 1)
	err := b1.Broadcast(100, []byte("test data"))
	if err != nil {
		t.Errorf("Error broadcasting: %v", err)
	}

	start := time.Now()
	for time.Since(start) < time.Second*5 {
		if strmDataResult.SeqNo == 0 {
			time.Sleep(100 * time.Millisecond)
		} else {
			break
		}
	}

	if string(strmDataResult.Data) != "test data" {
		t.Errorf("Expecting 'test data', got %v", strmDataResult.Data)
	}

	if len(n1.broadcasters) != 1 {
		t.Errorf("Should be 1 broadcaster in n1")
	}

	if len(n1.broadcasters[strmID].listeners) != 1 {
		t.Errorf("Should be 1 listener in b1")
	}

	if len(n2.relayers) != 1 {
		t.Errorf("Should be 1 relayer in n2")
	}

	if len(n2.relayers[strmID].listeners) != 1 {
		t.Errorf("Should be 1 listener in r2")
	}

	err = b1.Finish()
	// n1.DeleteBroadcaster(strmID)
	if err != nil {
		t.Errorf("Error when broadcasting Finish: %v", err)
	}

	//Wait for finish msg in n3
	start = time.Now()
	for time.Since(start) < time.Second*5 {
		if finishResult.StrmID == "" {
			time.Sleep(100 * time.Millisecond)
		} else {
			break
		}
	}

	if finishResult.StrmID != strmID {
		t.Errorf("Expecting finishResult to have strmID: %v, but got %v", strmID, finishResult)
	}

	if len(n1.broadcasters) != 0 {
		t.Errorf("Should have 0 broadcasters in n1")
	}

	if len(n2.relayers) != 0 {
		t.Errorf("Should have 0 relayers in n2")
	}
}

func TestSendTranscodeResponse(t *testing.T) {
	glog.Infof("\n\nTesting Handle Transcode Result...")
	n1, n2 := setupNodes()
	connectHosts(n1.NetworkNode.PeerHost, n2.NetworkNode.PeerHost)
	go n1.SetupProtocol()
	go n2.SetupProtocol()

	//Create the broadcaster, to capture the transcode result
	n2.GetBroadcaster("strmid")
	rc := make(chan map[string]string)
	n2.ReceivedTranscodeResponse("strmid", func(result map[string]string) {
		rc <- result
	})
	err := n1.SendTranscodeResponse(peer.IDHexEncode(n2.NetworkNode.Identity), "strmid", map[string]string{"strmid1": types.P240p30fps4x3.Name, "strmid2": types.P360p30fps4x3.Name})
	if err != nil {
		t.Errorf("Error sending transcode result: %v", err)
	}
	timer := time.NewTimer(time.Second * 3)
	select {
	case r := <-rc:
		if r["strmid1"] != types.P240p30fps4x3.Name {
			t.Errorf("Expecting %v, got %v", types.P240p30fps4x3.Name, r["strmid1"])
		}
		if r["strmid2"] != types.P360p30fps4x3.Name {
			t.Errorf("Expecting %v, got %v", types.P360p30fps4x3.Name, r["strmid2"])
		}
	case <-timer.C:
		t.Errorf("Timed out")
	}
}

func TestMasterPlaylist(t *testing.T) {
	glog.Infof("\n\nTesting handle master playlist")
	n1, n2 := setupNodes()
	connectHosts(n1.NetworkNode.PeerHost, n2.NetworkNode.PeerHost)
	go n1.SetupProtocol()

	mpl := m3u8.NewMasterPlaylist()
	pl, _ := m3u8.NewMediaPlaylist(10, 10)
	mpl.Append("test.m3u8", pl, m3u8.VariantParams{Bandwidth: 100000})

	err := n1.UpdateMasterPlaylist("test", mpl)
	if err != nil {
		t.Errorf("Error updating master playlist")
	}

	mplc, err := n2.GetMasterPlaylist(n1.GetNodeID(), "test")
	if err != nil {
		t.Errorf("Error getting master playlist: %v", err)
	}
	timer := time.NewTimer(time.Second * 3)
	select {
	case r := <-mplc:
		vars := r.Variants
		if len(vars) != 1 {
			t.Errorf("Expecting 1 variants, but got: %v - %v", len(vars), r)
		}
	case <-timer.C:
		t.Errorf("Timed out")
	}
}

func TestID(t *testing.T) {
	n1, _ := simpleNodes(15002, 15003)
	id := n1.Identity
	sid := peer.IDHexEncode(id)
	pid, err := peer.IDHexDecode(sid)
	if err != nil {
		t.Errorf("Error decoding id %v: %v", pid, err)
	}
}
