package server

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"database/sql/driver"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"shreeemp/database"

	gormpostgres "gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

const fakeDriverName = "server-test-postgres"

var fakeDriverOnce sync.Once
var fakeDBRegistry sync.Map

type fakeDBState struct {
	mu         sync.Mutex
	history    []database.MessageDB
	rooms      []string
	execSQL    []string
	querySQL   []string
	execErr    error
	queryErr   error
	lastOpened string
}

func ensureFakeDriver() {
	fakeDriverOnce.Do(func() {
		sql.Register(fakeDriverName, fakeDriver{})
	})
}

func setupFakeGormDB(t *testing.T, state *fakeDBState) func() {
	t.Helper()

	ensureFakeDriver()

	dsn := fmt.Sprintf("test-%d", time.Now().UnixNano())
	fakeDBRegistry.Store(dsn, state)

	sqlDB, err := sql.Open(fakeDriverName, dsn)
	if err != nil {
		t.Fatalf("open fake sql db: %v", err)
	}

	gdb, err := gorm.Open(gormpostgres.New(gormpostgres.Config{
		Conn:             sqlDB,
		WithoutReturning: true,
	}), &gorm.Config{
		Logger:                 gormlogger.Default.LogMode(gormlogger.Silent),
		SkipDefaultTransaction: true,
	})
	if err != nil {
		t.Fatalf("open gorm db: %v", err)
	}

	oldDB := database.DB
	database.DB = gdb

	return func() {
		database.DB = oldDB
		_ = sqlDB.Close()
		fakeDBRegistry.Delete(dsn)
	}
}

type fakeDriver struct{}

func (fakeDriver) Open(name string) (driver.Conn, error) {
	stateValue, ok := fakeDBRegistry.Load(name)
	if !ok {
		return nil, fmt.Errorf("unknown fake dsn %q", name)
	}

	state := stateValue.(*fakeDBState)
	state.mu.Lock()
	state.lastOpened = name
	state.mu.Unlock()

	return &fakeConn{state: state}, nil
}

type fakeConn struct {
	state *fakeDBState
}

func (c *fakeConn) Prepare(string) (driver.Stmt, error) {
	return nil, fmt.Errorf("prepare is not supported")
}

func (c *fakeConn) Close() error {
	return nil
}

func (c *fakeConn) Begin() (driver.Tx, error) {
	return nil, fmt.Errorf("transactions are not supported")
}

func (c *fakeConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	c.state.mu.Lock()
	c.state.execSQL = append(c.state.execSQL, query)
	err := c.state.execErr
	c.state.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return fakeResult{lastInsertID: 1, rowsAffected: 1}, nil
}

func (c *fakeConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	c.state.mu.Lock()
	c.state.querySQL = append(c.state.querySQL, query)
	err := c.state.queryErr
	history := append([]database.MessageDB(nil), c.state.history...)
	rooms := append([]string(nil), c.state.rooms...)
	c.state.mu.Unlock()
	if err != nil {
		return nil, err
	}

	lower := strings.ToLower(query)

	switch {
	case strings.Contains(lower, "distinct") && strings.Contains(lower, "room"):
		values := make([][]driver.Value, 0, len(rooms))
		for _, room := range rooms {
			values = append(values, []driver.Value{room})
		}
		return &fakeRows{columns: []string{"room"}, values: values}, nil
	case strings.Contains(lower, "from") && strings.Contains(lower, "messages"):
		values := make([][]driver.Value, 0, len(history))
		for _, msg := range history {
			values = append(values, []driver.Value{
				int64(msg.ID),
				msg.Room,
				msg.Sender,
				msg.Text,
				msg.CreatedAt,
			})
		}
		return &fakeRows{
			columns: []string{"id", "room", "sender", "text", "created_at"},
			values:  values,
		}, nil
	default:
		return nil, fmt.Errorf("unexpected query: %s", query)
	}
}

func (c *fakeConn) CheckNamedValue(*driver.NamedValue) error {
	return nil
}

var _ driver.Conn = (*fakeConn)(nil)
var _ driver.ExecerContext = (*fakeConn)(nil)
var _ driver.QueryerContext = (*fakeConn)(nil)
var _ driver.NamedValueChecker = (*fakeConn)(nil)

type fakeResult struct {
	lastInsertID int64
	rowsAffected int64
}

func (r fakeResult) LastInsertId() (int64, error) {
	return r.lastInsertID, nil
}

func (r fakeResult) RowsAffected() (int64, error) {
	return r.rowsAffected, nil
}

type fakeRows struct {
	columns []string
	values  [][]driver.Value
	idx     int
}

func (r *fakeRows) Columns() []string {
	return r.columns
}

func (r *fakeRows) Close() error {
	return nil
}

func (r *fakeRows) Next(dest []driver.Value) error {
	if r.idx >= len(r.values) {
		return io.EOF
	}

	copy(dest, r.values[r.idx])
	r.idx++
	return nil
}

var _ driver.Rows = (*fakeRows)(nil)

type recordingAddr string

func (a recordingAddr) Network() string { return "recording" }
func (a recordingAddr) String() string  { return string(a) }

type recordingConn struct {
	mu     sync.Mutex
	buf    strings.Builder
	closed bool
}

func (c *recordingConn) Read(_ []byte) (int, error) {
	return 0, io.EOF
}

func (c *recordingConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.WriteString(string(p))
}

func (c *recordingConn) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	return nil
}

func (c *recordingConn) LocalAddr() net.Addr  { return recordingAddr("local") }
func (c *recordingConn) RemoteAddr() net.Addr { return recordingAddr("remote") }
func (c *recordingConn) SetDeadline(time.Time) error {
	return nil
}
func (c *recordingConn) SetReadDeadline(time.Time) error {
	return nil
}
func (c *recordingConn) SetWriteDeadline(time.Time) error {
	return nil
}

func (c *recordingConn) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

func TestCleanStringFiltersAndLimits(t *testing.T) {
	got := cleanString(" \thello\nworld\x00 ")
	if got != "helloworld" {
		t.Fatalf("cleanString() = %q, want %q", got, "helloworld")
	}

	long := strings.Repeat("a", 600)
	got = cleanString(long)
	if got != strings.Repeat("a", 500) {
		t.Fatalf("cleanString() length = %d runes, want 500", utf8.RuneCountInString(got))
	}
}

func TestNewHubInitializesEmptyRooms(t *testing.T) {
	hub := NewHub()
	if hub == nil {
		t.Fatal("NewHub() returned nil")
	}
	if len(hub.rooms) != 0 {
		t.Fatalf("NewHub() rooms len = %d, want 0", len(hub.rooms))
	}
}

func TestJoinRoomLoadsHistoryAndTracksClient(t *testing.T) {
	state := &fakeDBState{
		history: []database.MessageDB{
			{
				ID:        2,
				Room:      "general",
				Sender:    "Bob",
				Text:      "second",
				CreatedAt: time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC),
			},
			{
				ID:        1,
				Room:      "general",
				Sender:    "Alice",
				Text:      "first",
				CreatedAt: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
			},
		},
	}
	cleanup := setupFakeGormDB(t, state)
	defer cleanup()

	hub := NewHub()
	conn := &recordingConn{}
	client := &Client{conn: conn, name: "Tester"}

	hub.JoinRoom("general", client)

	if client.room != "general" {
		t.Fatalf("client.room = %q, want %q", client.room, "general")
	}

	hub.mu.Lock()
	_, ok := hub.rooms["general"][client]
	hub.mu.Unlock()
	if !ok {
		t.Fatal("client was not added to the room")
	}

	output := conn.String()
	wantOrder := []string{
		"Alice: first\n",
		"Bob: second\n",
		"—— Выше история сообщений ——\n\n",
		"— Tester присоединился к комнате general —\n",
	}

	last := -1
	for _, want := range wantOrder {
		idx := strings.Index(output, want)
		if idx < 0 {
			t.Fatalf("output %q does not contain %q", output, want)
		}
		if idx < last {
			t.Fatalf("output %q has wrong order for %q", output, want)
		}
		last = idx
	}
}

func TestJoinRoomMovesClientFromPreviousRoomAndNotifiesOldRoom(t *testing.T) {
	oldDB := database.DB
	database.DB = nil
	t.Cleanup(func() {
		database.DB = oldDB
	})

	hub := NewHub()
	movingConn := &recordingConn{}
	stayingConn := &recordingConn{}
	movingClient := &Client{conn: movingConn, name: "Mover", room: "old"}
	stayingClient := &Client{conn: stayingConn, name: "Stayer", room: "old"}

	hub.rooms["old"] = map[*Client]bool{
		movingClient:  true,
		stayingClient: true,
	}

	hub.JoinRoom("new", movingClient)

	if movingClient.room != "new" {
		t.Fatalf("movingClient.room = %q, want new", movingClient.room)
	}
	if _, exists := hub.rooms["old"][movingClient]; exists {
		t.Fatal("moving client was not removed from the old room")
	}
	if _, exists := hub.rooms["new"][movingClient]; !exists {
		t.Fatal("moving client was not added to the new room")
	}
	if !strings.Contains(stayingConn.String(), "— Mover покинул комнату —\n") {
		t.Fatalf("staying client output = %q, want leave notification", stayingConn.String())
	}
	if !strings.Contains(movingConn.String(), "— Mover присоединился к комнате new —\n") {
		t.Fatalf("moving client output = %q, want join notification", movingConn.String())
	}
}

func TestJoinRoomContinuesWhenHistoryQueryFails(t *testing.T) {
	state := &fakeDBState{queryErr: fmt.Errorf("query failed")}
	cleanup := setupFakeGormDB(t, state)
	defer cleanup()

	hub := NewHub()
	conn := &recordingConn{}
	client := &Client{conn: conn, name: "Tester"}

	hub.JoinRoom("general", client)

	if client.room != "general" {
		t.Fatalf("client.room = %q, want general", client.room)
	}
	if !strings.Contains(conn.String(), "— Tester присоединился к комнате general —\n") {
		t.Fatalf("output = %q, want join notification", conn.String())
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	if len(state.querySQL) == 0 {
		t.Fatal("history query was not attempted")
	}
	if !strings.Contains(strings.ToLower(state.querySQL[0]), "messages") {
		t.Fatalf("history query = %q, want messages query", state.querySQL[0])
	}
}

func TestGetRoomsWithCountsReturnsDefaultsAndDBRooms(t *testing.T) {
	state := &fakeDBState{
		rooms: []string{"space", "general", "space", ""},
	}
	cleanup := setupFakeGormDB(t, state)
	defer cleanup()

	hub := NewHub()

	clientA := &Client{name: "A"}
	clientB := &Client{name: "B"}
	clientC := &Client{name: "C"}

	hub.rooms["general"] = map[*Client]bool{clientA: true, clientB: true}
	hub.rooms["random"] = map[*Client]bool{clientC: true}
	hub.rooms["space"] = map[*Client]bool{clientA: true}

	raw := hub.GetRoomsWithCounts()
	if !strings.HasPrefix(raw, "ROOMS_LIST:") {
		t.Fatalf("GetRoomsWithCounts() = %q, want ROOMS_LIST prefix", raw)
	}

	parts := strings.Split(strings.TrimPrefix(raw, "ROOMS_LIST:"), ",")
	got := make(map[string]string, len(parts))
	for _, part := range parts {
		if part == "" {
			continue
		}
		room, count, ok := strings.Cut(part, ":")
		if !ok {
			t.Fatalf("invalid room count entry %q", part)
		}
		got[room] = count
	}

	want := map[string]string{
		"general": "2",
		"crypto":  "0",
		"random":  "1",
		"gamedev": "0",
		"space":   "1",
	}

	for room, wantCount := range want {
		if got[room] != wantCount {
			t.Fatalf("room %q count = %q, want %q; raw=%q", room, got[room], wantCount, raw)
		}
	}
}

func TestGetRoomsWithCountsWorksWithoutDB(t *testing.T) {
	oldDB := database.DB
	database.DB = nil
	t.Cleanup(func() {
		database.DB = oldDB
	})

	hub := NewHub()
	client := &Client{name: "A"}
	hub.rooms["live"] = map[*Client]bool{client: true}

	got := parseRoomsList(t, hub.GetRoomsWithCounts())

	want := map[string]string{
		"general": "0",
		"crypto":  "0",
		"random":  "0",
		"gamedev": "0",
		"live":    "1",
	}
	for room, wantCount := range want {
		if got[room] != wantCount {
			t.Fatalf("room %q count = %q, want %q", room, got[room], wantCount)
		}
	}
}

func TestGetRoomsWithCountsContinuesWhenDBQueryFails(t *testing.T) {
	state := &fakeDBState{queryErr: fmt.Errorf("query failed")}
	cleanup := setupFakeGormDB(t, state)
	defer cleanup()

	hub := NewHub()
	client := &Client{name: "A"}
	hub.rooms["live"] = map[*Client]bool{client: true}

	got := parseRoomsList(t, hub.GetRoomsWithCounts())
	if got["live"] != "1" {
		t.Fatalf("live room count = %q, want 1", got["live"])
	}
	if got["general"] != "0" {
		t.Fatalf("default room count = %q, want 0", got["general"])
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	if len(state.querySQL) == 0 {
		t.Fatal("rooms query was not attempted")
	}
	if !strings.Contains(strings.ToLower(state.querySQL[0]), "distinct") {
		t.Fatalf("rooms query = %q, want distinct query", state.querySQL[0])
	}
}

func TestBroadcastWritesToClientsAndPersistsMessage(t *testing.T) {
	state := &fakeDBState{}
	cleanup := setupFakeGormDB(t, state)
	defer cleanup()

	hub := NewHub()
	conn := &recordingConn{}
	client := &Client{conn: conn, name: "Alice", room: "general"}

	hub.rooms["general"] = map[*Client]bool{client: true}

	hub.Broadcast("general", client, "hello")

	if got := conn.String(); got != "Alice: hello\n" {
		t.Fatalf("broadcast output = %q, want %q", got, "Alice: hello\n")
	}
}

func TestBroadcastNoopsForEmptyAndMissingRoom(t *testing.T) {
	state := &fakeDBState{}
	cleanup := setupFakeGormDB(t, state)
	defer cleanup()

	hub := NewHub()
	conn := &recordingConn{}
	client := &Client{conn: conn, name: "Alice", room: "general"}

	hub.Broadcast("", client, "ignored")
	hub.Broadcast("missing", client, "ignored")

	if got := conn.String(); got != "" {
		t.Fatalf("broadcast output = %q, want empty", got)
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	if len(state.execSQL) != 0 {
		t.Fatalf("exec count = %d, want 0", len(state.execSQL))
	}
}

func TestBroadcastWorksWithoutDB(t *testing.T) {
	oldDB := database.DB
	database.DB = nil
	t.Cleanup(func() {
		database.DB = oldDB
	})

	hub := NewHub()
	conn := &recordingConn{}
	client := &Client{conn: conn, name: "Alice", room: "general"}
	hub.rooms["general"] = map[*Client]bool{client: true}

	hub.Broadcast("general", client, "hello")

	if got := conn.String(); got != "Alice: hello\n" {
		t.Fatalf("broadcast output = %q, want %q", got, "Alice: hello\n")
	}
}

func TestBroadcastContinuesWhenPersistFails(t *testing.T) {
	state := &fakeDBState{execErr: fmt.Errorf("insert failed")}
	cleanup := setupFakeGormDB(t, state)
	defer cleanup()

	hub := NewHub()
	conn := &recordingConn{}
	client := &Client{conn: conn, name: "Alice", room: "general"}
	hub.rooms["general"] = map[*Client]bool{client: true}

	hub.Broadcast("general", client, "hello")

	if got := conn.String(); got != "Alice: hello\n" {
		t.Fatalf("broadcast output = %q, want %q", got, "Alice: hello\n")
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	if len(state.execSQL) != 1 {
		t.Fatalf("exec count = %d, want 1", len(state.execSQL))
	}
	if !strings.Contains(strings.ToLower(state.execSQL[0]), "insert") {
		t.Fatalf("exec SQL = %q, want insert", state.execSQL[0])
	}
}

func TestBroadcastRemovesClientAfterWriteError(t *testing.T) {
	state := &fakeDBState{}
	cleanup := setupFakeGormDB(t, state)
	defer cleanup()

	hub := NewHub()
	badConn := &failingConn{}
	client := &Client{conn: badConn, name: "Alice", room: "general"}

	hub.rooms["general"] = map[*Client]bool{client: true}

	hub.Broadcast("general", client, "hello")

	waitFor(t, time.Second, func() bool {
		return client.room == "" && len(hub.rooms) == 0
	})
}

func TestRemoveClientNotifiesRemainingClients(t *testing.T) {
	hub := NewHub()
	leavingConn := &recordingConn{}
	stayingConn := &recordingConn{}
	leavingClient := &Client{conn: leavingConn, name: "Alice", room: "general"}
	stayingClient := &Client{conn: stayingConn, name: "Bob", room: "general"}
	hub.rooms["general"] = map[*Client]bool{
		leavingClient: true,
		stayingClient: true,
	}

	hub.RemoveClient(leavingClient)

	if leavingClient.room != "" {
		t.Fatalf("leavingClient.room = %q, want empty", leavingClient.room)
	}
	if _, exists := hub.rooms["general"][leavingClient]; exists {
		t.Fatal("leaving client was not removed")
	}
	if !strings.Contains(stayingConn.String(), "— Alice покинул комнату —\n") {
		t.Fatalf("staying client output = %q, want leave notification", stayingConn.String())
	}
}

func TestRemoveClientDeletesEmptyRoom(t *testing.T) {
	hub := NewHub()
	client := &Client{name: "Alice", room: "general"}
	hub.rooms["general"] = map[*Client]bool{client: true}

	hub.RemoveClient(client)

	if client.room != "" {
		t.Fatalf("client.room = %q, want empty", client.room)
	}
	if _, exists := hub.rooms["general"]; exists {
		t.Fatal("room was not deleted after last client left")
	}
}

func TestHandleServerClientHTTPBlock(t *testing.T) {
	hub := NewHub()
	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()

	done := make(chan struct{})
	go func() {
		handleServerClient(serverConn, hub)
		close(done)
	}()

	if _, err := io.WriteString(clientConn, "GET "); err != nil {
		t.Fatalf("write request: %v", err)
	}

	clientConn.Close()

	waitFor(t, time.Second, func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	})
}

func TestHandleServerClientAnonymousNicknameAndEmptyMessages(t *testing.T) {
	oldDB := database.DB
	database.DB = nil
	t.Cleanup(func() {
		database.DB = oldDB
	})

	hub := NewHub()
	serverConn, clientConn := net.Pipe()

	var (
		mu      sync.Mutex
		lines   []string
		readErr error
	)
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		scanner := bufio.NewScanner(clientConn)
		for scanner.Scan() {
			mu.Lock()
			lines = append(lines, scanner.Text())
			mu.Unlock()
		}
		mu.Lock()
		readErr = scanner.Err()
		mu.Unlock()
	}()

	done := make(chan struct{})
	go func() {
		handleServerClient(serverConn, hub)
		close(done)
	}()

	if _, err := fmt.Fprintln(clientConn, "   "); err != nil {
		t.Fatalf("write empty nickname: %v", err)
	}
	if _, err := fmt.Fprintln(clientConn, "   "); err != nil {
		t.Fatalf("write empty message: %v", err)
	}
	if _, err := fmt.Fprintln(clientConn, "/join general"); err != nil {
		t.Fatalf("write join: %v", err)
	}

	waitFor(t, time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return containsLine(lines, "— Anonymouse присоединился к комнате general —") &&
			containsLine(lines, "Вы перешли в комнату: general")
	})

	clientConn.Close()

	waitFor(t, time.Second, func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	})
	waitFor(t, time.Second, func() bool {
		select {
		case <-readDone:
			return true
		default:
			return false
		}
	})

	mu.Lock()
	defer mu.Unlock()
	if isUnexpectedScannerErr(readErr) {
		t.Fatalf("read server responses: %v", readErr)
	}
}

func TestHandleServerClientCommands(t *testing.T) {
	state := &fakeDBState{
		history: []database.MessageDB{
			{
				ID:        1,
				Room:      "general",
				Sender:    "Alice",
				Text:      "welcome",
				CreatedAt: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
			},
		},
		rooms: []string{"general"},
	}
	cleanup := setupFakeGormDB(t, state)
	defer cleanup()

	hub := NewHub()
	serverConn, clientConn := net.Pipe()

	var (
		mu      sync.Mutex
		lines   []string
		readErr error
	)
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		scanner := bufio.NewScanner(clientConn)
		for scanner.Scan() {
			mu.Lock()
			lines = append(lines, scanner.Text())
			mu.Unlock()
		}
		mu.Lock()
		readErr = scanner.Err()
		mu.Unlock()
	}()

	done := make(chan struct{})
	go func() {
		handleServerClient(serverConn, hub)
		close(done)
	}()

	writeLine := func(s string) {
		t.Helper()
		if _, err := fmt.Fprintln(clientConn, s); err != nil {
			t.Fatalf("write %q: %v", s, err)
		}
	}

	writeLine("Tester")
	writeLine("/join general")
	writeLine("/setnick Neo")
	writeLine("hello")
	writeLine("/leave")

	waitFor(t, time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return containsLine(lines, "CMD:NICK_UPDATED:Neo") &&
			containsLine(lines, "Neo: hello") &&
			containsLine(lines, "Alice: welcome") &&
			containsLine(lines, "—— Выше история сообщений ——") &&
			containsLine(lines, "— Пользователь Tester изменил имя на Neo —") &&
			containsLine(lines, "Вы перешли в комнату: general") &&
			countRoomsListResponses(lines) >= 2
	})

	clientConn.Close()

	waitFor(t, time.Second, func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	})

	waitFor(t, time.Second, func() bool {
		select {
		case <-readDone:
			return true
		default:
			return false
		}
	})

	hub.mu.Lock()
	if len(hub.rooms) != 0 {
		hub.mu.Unlock()
		t.Fatalf("hub.rooms len = %d, want 0", len(hub.rooms))
	}
	hub.mu.Unlock()

	mu.Lock()
	linesSnapshot := append([]string(nil), lines...)
	err := readErr
	mu.Unlock()
	if isUnexpectedScannerErr(err) {
		t.Fatalf("read server responses: %v", err)
	}

	roomsListCount := countRoomsListResponses(linesSnapshot)
	if roomsListCount < 2 {
		t.Fatalf("ROOMS_LIST response count = %d, want at least 2; lines=%q", roomsListCount, linesSnapshot)
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	if len(state.execSQL) != 1 {
		t.Fatalf("message insert count = %d, want 1", len(state.execSQL))
	}
	if !strings.Contains(strings.ToLower(state.execSQL[0]), "insert") {
		t.Fatalf("exec SQL = %q, want insert", state.execSQL[0])
	}
}

func TestCreateListenerWithoutCertsUsesTCP(t *testing.T) {
	oldListenTCP := listenTCP
	oldListenTLS := listenTLS
	t.Cleanup(func() {
		listenTCP = oldListenTCP
		listenTLS = oldListenTLS
	})

	tcpListener := &fakeListener{}
	listenTCP = func(network, addr string) (net.Listener, error) {
		if network != "tcp" {
			t.Fatalf("listenTCP network = %q, want tcp", network)
		}
		if addr != ":0" {
			t.Fatalf("listenTCP addr = %q, want :0", addr)
		}
		return tcpListener, nil
	}
	listenTLS = func(network, addr string, _ *tls.Config) (net.Listener, error) {
		t.Fatalf("listenTLS should not be called in TCP test")
		return nil, nil
	}

	listener, err := createListener(0)
	if err != nil {
		t.Fatalf("createListener() error = %v", err)
	}
	defer listener.Close()

	if listener == nil {
		t.Fatal("createListener() returned nil listener")
	}
	if listener != tcpListener {
		t.Fatal("createListener() did not return the TCP listener")
	}
}

func TestCreateListenerWithCertsUsesTLS(t *testing.T) {
	oldListenTCP := listenTCP
	oldListenTLS := listenTLS
	t.Cleanup(func() {
		listenTCP = oldListenTCP
		listenTLS = oldListenTLS
	})

	tempDir := t.TempDir()
	certsDir := filepath.Join(tempDir, "certs")
	if err := os.MkdirAll(certsDir, 0o755); err != nil {
		t.Fatalf("mkdir certs: %v", err)
	}

	certPEM, keyPEM := mustGenerateSelfSignedCert(t)
	if err := os.WriteFile(filepath.Join(certsDir, "server.crt"), certPEM, 0o644); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(filepath.Join(certsDir, "server.key"), keyPEM, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}

	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(tempDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(oldWD)
	})

	tlsListener := &fakeListener{}
	listenTCP = func(network, addr string) (net.Listener, error) {
		t.Fatalf("listenTCP should not be called in TLS test")
		return nil, nil
	}
	listenTLS = func(network, addr string, _ *tls.Config) (net.Listener, error) {
		if network != "tcp" {
			t.Fatalf("listenTLS network = %q, want tcp", network)
		}
		if addr != ":0" {
			t.Fatalf("listenTLS addr = %q, want :0", addr)
		}
		return tlsListener, nil
	}

	listener, err := createListener(0)
	if err != nil {
		t.Fatalf("createListener() error = %v", err)
	}
	defer listener.Close()

	if listener != tlsListener {
		t.Fatal("createListener() did not return the TLS listener")
	}
}

func TestCreateListenerWithInvalidCertsFails(t *testing.T) {
	tempDir := t.TempDir()
	certsDir := filepath.Join(tempDir, "certs")
	if err := os.MkdirAll(certsDir, 0o755); err != nil {
		t.Fatalf("mkdir certs: %v", err)
	}
	if err := os.WriteFile(filepath.Join(certsDir, "server.crt"), []byte("bad"), 0o644); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(filepath.Join(certsDir, "server.key"), []byte("bad"), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}

	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(tempDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(oldWD)
	})

	_, err = createListener(0)
	if err == nil {
		t.Fatal("createListener() with invalid certs expected error")
	}
}

func mustGenerateSelfSignedCert(t *testing.T) ([]byte, []byte) {
	t.Helper()

	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		t.Fatalf("serial: %v", err)
	}

	template := x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "localhost",
			Organization: []string{"shreeemp"},
		},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
	}

	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)})
	return certPEM, keyPEM
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timeout waiting for condition")
}

func containsLine(lines []string, want string) bool {
	for _, line := range lines {
		if line == want {
			return true
		}
	}
	return false
}

func countRoomsListResponses(lines []string) int {
	count := 0
	for _, line := range lines {
		if strings.HasPrefix(line, "ROOMS_LIST:") {
			count++
		}
	}
	return count
}

func isUnexpectedScannerErr(err error) bool {
	return err != nil && !errors.Is(err, io.ErrClosedPipe) && !errors.Is(err, net.ErrClosed)
}

func parseRoomsList(t *testing.T, raw string) map[string]string {
	t.Helper()

	if !strings.HasPrefix(raw, "ROOMS_LIST:") {
		t.Fatalf("rooms list = %q, want ROOMS_LIST prefix", raw)
	}

	parts := strings.Split(strings.TrimPrefix(raw, "ROOMS_LIST:"), ",")
	rooms := make(map[string]string, len(parts))
	for _, part := range parts {
		if part == "" {
			continue
		}
		room, count, ok := strings.Cut(part, ":")
		if !ok {
			t.Fatalf("invalid room count entry %q", part)
		}
		rooms[room] = count
	}
	return rooms
}

type failingConn struct{}

func (f *failingConn) Read(_ []byte) (int, error)       { return 0, io.EOF }
func (f *failingConn) Write(_ []byte) (int, error)      { return 0, fmt.Errorf("write failed") }
func (f *failingConn) Close() error                     { return nil }
func (f *failingConn) LocalAddr() net.Addr              { return recordingAddr("local") }
func (f *failingConn) RemoteAddr() net.Addr             { return recordingAddr("remote") }
func (f *failingConn) SetDeadline(time.Time) error      { return nil }
func (f *failingConn) SetReadDeadline(time.Time) error  { return nil }
func (f *failingConn) SetWriteDeadline(time.Time) error { return nil }

type fakeListener struct {
	addr     net.Addr
	closed   bool
	accepted bool
}

func (l *fakeListener) Accept() (net.Conn, error) {
	l.accepted = true
	return nil, fmt.Errorf("accept not supported")
}

func (l *fakeListener) Close() error {
	l.closed = true
	return nil
}

func (l *fakeListener) Addr() net.Addr {
	if l.addr != nil {
		return l.addr
	}
	return recordingAddr("fake-listener")
}
