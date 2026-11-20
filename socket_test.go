package main

import (
	"compress/zlib"
	"encoding/json"
	"io"
	"net"
	"strconv"
	"testing"
	"time"
)

func TestActiveGameSessionReceivesWorldMarketRefresh(t *testing.T) {
	store := newWorldMarketTestStore(t)
	player, err := store.CreatePlayer("socket-market-player", "Island", "socket-market-device")
	if err != nil {
		t.Fatal(err)
	}

	serverConn, clientConn := net.Pipe()
	if err := clientConn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		handleMessage(serverConn, store)
	}()
	t.Cleanup(func() {
		clientConn.Close()
		select {
		case <-serverDone:
		case <-time.After(time.Second):
			t.Error("socket handler did not stop after the client disconnected")
		}
	})

	clientWriter := zlib.NewWriter(clientConn)
	clientReaderReady := make(chan struct {
		reader io.ReadCloser
		err    error
	}, 1)
	go func() {
		reader, err := zlib.NewReader(clientConn)
		clientReaderReady <- struct {
			reader io.ReadCloser
			err    error
		}{reader: reader, err: err}
	}()

	writeSocketTestMessage(t, clientWriter, ClientMessage{
		Cmd:     Connect,
		Request: 1,
		Data:    map[string]interface{}{"cid": player.UUID},
	})
	clientReaderResult := <-clientReaderReady
	if clientReaderResult.err != nil {
		t.Fatalf("create client zlib reader: %v", clientReaderResult.err)
	}
	clientReader := clientReaderResult.reader
	connectResponse := readSocketTestMessage(t, clientReader)
	if connectResponse["cmd"] != string(Connect) {
		t.Fatalf("connect response command = %v, want %q", connectResponse["cmd"], Connect)
	}

	writeSocketTestMessage(t, clientWriter, ClientMessage{
		Cmd:     OnNewTransactionContext,
		Request: 2,
		Data:    map[string]interface{}{"tcId": 42, "timelineId": 1},
	})
	readSocketTestMessage(t, clientReader)
	sessionConfiguration := readSocketTestMessage(t, clientReader)
	sessionData, ok := sessionConfiguration["data"].(map[string]interface{})
	if !ok || sessionData["type"] != "sessionConfiguration" {
		t.Fatalf("session configuration response = %v", sessionConfiguration)
	}

	if err := store.refreshWorldMarketCache(player.UUID); err != nil {
		t.Fatalf("refresh world market: %v", err)
	}
	if count, err := store.pushWorldMarketCacheUpdate(player.UUID); err != nil || count != 1 {
		t.Fatalf("push refreshed market: sessions=%d, err=%v; want one session and no error", count, err)
	}

	push := readSocketTestMessage(t, clientReader)
	pushData, ok := push["data"].(map[string]interface{})
	if push["cmd"] != string(LuaSessionMessage) || !ok || pushData["type"] != "requestDocumentsResponse" {
		t.Fatalf("world market push = %v", push)
	}
	if pushData["tcId"] != float64(42) {
		t.Fatalf("pushed transaction context ID = %v, want 42", pushData["tcId"])
	}
	updates, ok := pushData["updates"].([]interface{})
	if !ok || len(updates) != 1 {
		t.Fatalf("pushed fragment updates = %v, want one update", pushData["updates"])
	}
}

func writeSocketTestMessage(t *testing.T, writer *zlib.Writer, message ClientMessage) {
	t.Helper()
	data, err := json.Marshal(message)
	if err != nil {
		t.Fatalf("encode socket message: %v", err)
	}
	if err := writeFrame(writer, data); err != nil {
		t.Fatalf("write socket message: %v", err)
	}
}

func readSocketTestMessage(t *testing.T, reader io.Reader) map[string]interface{} {
	t.Helper()
	prefix := make([]byte, 7)
	if _, err := io.ReadFull(reader, prefix); err != nil {
		t.Fatalf("read socket frame length: %v", err)
	}
	length, err := strconv.Atoi(string(prefix))
	if err != nil {
		t.Fatalf("decode socket frame length %q: %v", prefix, err)
	}
	data := make([]byte, length)
	if _, err := io.ReadFull(reader, data); err != nil {
		t.Fatalf("read socket frame: %v", err)
	}
	var message map[string]interface{}
	if err := json.Unmarshal(data, &message); err != nil {
		t.Fatalf("decode socket message: %v", err)
	}
	return message
}
