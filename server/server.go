package server

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"unicode"

	"shreeemp/database"
)

var listenTCP = net.Listen
var listenTLS = tls.Listen

// Run запускает сервер на указанном порту
func Run(port int) {
	listener, err := createListener(port)
	if err != nil {
		log.Fatalf("[SERVER FATAL] Не удалось инициализировать сервер: %v", err)
	}
	defer listener.Close()

	database.InitDB()

	hub := NewHub()

	// Канал, который мы закроем, когда сервер должен завершить работу
	serverDone := make(chan struct{})

	// Фоновая горутина для приема соединений
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				select {
				case <-serverDone:
					// Если канал закрыт, значит остановка была преднамеренной. Уходим тихо.
					return
				default:
					log.Println("[SERVER ERROR] Ошибка Accept:", err)
					continue
				}
			}
			go handleServerClient(conn, hub)
		}
	}()

	// Ожидаем системного сигнала в главном потоке (блокируемся тут)
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	log.Println("[SERVER] Получен сигнал остановки. Инициируем Graceful Shutdown...")

	// Порядок очень важен:
	close(serverDone) // 1. Сначала говорим нашей горутине: "Мы закрываемся"
	listener.Close()  // 2. Только теперь пинаем listener, чтобы Accept() разблокировался

	// ЗАКРЫВАЕМ БАЗУ ДАННЫХ:
	database.CloseDB()

	log.Println("[SERVER] Сервер успешно завершил работу.")
}

// createListener решает, какой листенер создать: TLS (рабочий) или TCP (разработка)
func createListener(port int) (net.Listener, error) {
	addr := fmt.Sprintf(":%d", port)
	certPath := "certs/server.crt"
	keyPath := "certs/server.key"

	// Проверяем наличие сертификатов на диске
	_, errCert := os.Stat(certPath)
	_, errKey := os.Stat(keyPath)

	if errCert == nil && errKey == nil {
		// СЦЕНАРИЙ А: Сертификаты найдены -> запускаем TLS
		log.Printf("[SERVER] Рабочий режим. Запуск безопасного TLS 1.3 сервера на %s...", addr)

		cer, err := tls.LoadX509KeyPair(certPath, keyPath)
		if err != nil {
			return nil, fmt.Errorf("ошибка загрузки сертификатов: %w", err)
		}

		config := &tls.Config{
			Certificates: []tls.Certificate{cer},
			MinVersion:   tls.VersionTLS13,
		}

		return listenTLS("tcp", addr, config)
	}

	// СЦЕНАРИЙ Б: Сертификатов нет -> запускаем обычный TCP
	log.Printf("[SERVER WARNING] Режим разработки. Сертификаты не найдены. Запуск сервера на обычном TCP на %s...", addr)
	return listenTCP("tcp", addr)
}

// Client описывает подключенного клиента
type Client struct {
	conn net.Conn
	name string
	room string // Имя текущей комнаты, в которой находится клиент
}

// Hub управляет всеми комнатами и рассылкой сообщений
type Hub struct {
	// Карта комнат. Ключ — название комнаты, значение — карта клиентов в этой комнате
	rooms map[string]map[*Client]bool
	mu    sync.Mutex // Этот мьютекс защищает карту rooms
}

// NewHub создает и инициализирует новый хаб
func NewHub() *Hub {
	return &Hub{
		rooms: make(map[string]map[*Client]bool),
	}
}

func handleServerClient(conn net.Conn, hub *Hub) {
	defer conn.Close()

	// Создаем буферизированный читатель для сокета
	reader := bufio.NewReader(conn)

	// Заглядываем в первые 4 байта подключения
	// Peek не продвигает указатель чтения, данные остаются в буфере для обычных клиентов
	preview, err := reader.Peek(4)
	if err == nil {
		firstBytes := string(preview)

		// Если это HTTP-методы, которые шлют сканеры или браузеры
		if strings.HasPrefix(firstBytes, "GET ") ||
			strings.HasPrefix(firstBytes, "POST") ||
			strings.HasPrefix(firstBytes, "HEAD") {

			log.Printf("[SERVER BLOCK] Сброшено HTTP-подключение сканера с адреса: %s", conn.RemoteAddr())
			conn.Close()
			return
		}
	}

	// 1. Создаем сканер для всего времени жизни соединения
	scanner := bufio.NewScanner(reader)

	// 2. Читаем строго первую строчку — это никнейм
	var nickname string
	if scanner.Scan() {
		// Очищаем от мусора
		nickname = cleanString(scanner.Text())
	}

	// Если клиент отключился сразу или прислал пустую строку
	if nickname == "" {
		nickname = "Anonymouse"
	}

	// 3. Инициализируем клиента с полученным ником
	client := &Client{
		conn: conn,
		name: nickname,
	}
	defer hub.RemoveClient(client)

	// Отправляем список комнат клиенту
	_, _ = fmt.Fprintf(conn, "%s\n", hub.GetRoomsWithCounts())

	log.Printf("[SERVER] Пользователь %s (%s) успешно авторизован", client.name, conn.RemoteAddr().String())

	for scanner.Scan() {
		text := cleanString(scanner.Text())
		if text == "" {
			continue
		}

		if text == "/leave" {
			if client.room != "" {
				oldRoom := client.room
				log.Printf("[SERVER] Пользователь %s покинул комнату %s", client.name, oldRoom)

				// Вызываем наш метод хаба, который удаляет клиента из комнаты и шлет уведомление остальным
				hub.RemoveClient(client)

				// Отправляем клиенту в ответ свежий список комнат с актуальным онлайном
				_, _ = fmt.Fprintf(conn, "%s\n", hub.GetRoomsWithCounts())
			}
			continue
		}

		if newNick, ok := strings.CutPrefix(text, "/setnick "); ok {
			if newNick != "" {
				oldNick := client.name

				// Защиты ради: здесь мьютекс хаба не нужен для изменения переменной клиента,
				// но так как мы будем делать Broadcast, хаб сам заблокирует то, что нужно.
				client.name = newNick

				// Оповещаем комнату о переименовании
				notification := fmt.Sprintf("— Пользователь %s изменил имя на %s —\n", oldNick, newNick)
				hub.Broadcast(client.room, nil, notification)

				// Отправляем скрытую команду клиенту.
				// Добавляем префикс CMD:NICK_UPDATED:, чтобы клиент опознал её.
				// Обязательно добавляем \n в конце, чтобы scanner.Scan() на клиенте её прочитал!
				cmdToClient := fmt.Sprintf("CMD:NICK_UPDATED:%s\n", newNick)
				_, _ = client.conn.Write([]byte(cmdToClient))

				log.Printf("[SERVER] %s переименовался в %s", oldNick, newNick)

				continue
			}
		}

		// Логика переключения комнат по команде, например: /join ИМЯ_КОМНАТЫ
		if s, ok := strings.CutPrefix(text, "/join "); ok {
			newRoom := s
			if newRoom != "" {
				hub.JoinRoom(newRoom, client)
				_, _ = fmt.Fprintf(conn, "Вы перешли в комнату: %s\n", newRoom)
				roomsList := hub.GetRoomsWithCounts()
				if f, err := os.OpenFile("/tmp/shreeemp_debug.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644); err == nil {
					fmt.Fprintf(f, "[SERVER] Sent: %s\n", roomsList)
					f.Close()
				}
				_, _ = fmt.Fprintf(conn, "%s\n", roomsList)
				continue
			}
		}

		// Рассылка обычного сообщения
		log.Printf("[%s в %s]: %s", client.name, client.room, text)
		hub.Broadcast(client.room, client, text)
	}

	// Проверяем ошибку этого же сканера при выходе
	if err := scanner.Err(); err != nil {
		log.Printf("[SERVER ERROR] Ошибка чтения клиента %s: %v", client.name, err)
	}
}

func (h *Hub) JoinRoom(roomName string, client *Client) {
	var leftRoom string
	var leftRoomClients []*Client

	h.mu.Lock()
	if client.room != "" {
		leftRoom, leftRoomClients = h.leaveRoomInternal(client)
	}

	if _, exists := h.rooms[roomName]; !exists {
		h.rooms[roomName] = make(map[*Client]bool)
	}
	h.rooms[roomName][client] = true
	client.room = roomName
	h.mu.Unlock()

	if leftRoom != "" {
		h.writeToClients(leftRoomClients, fmt.Sprintf("— %s покинул комнату —\n", client.name))
	}

	// 2. ОТПРАВКА ИСТОРИИ ИЗ БД ДО ПРИВЕТСТВИЯ
	var history []database.MessageDB
	// Берем последние 50 сообщений, отсортированных по дате создания
	if database.DB != nil {
		result := database.DB.Where("room = ?", roomName).Order("created_at desc").Limit(50).Find(&history)
		if result.Error != nil {
			log.Printf("[DB ERROR] Не удалось загрузить историю комнаты %s: %v", roomName, result.Error)
		}
	}
	if len(history) > 0 {
		// Так как мы достали их в обратном порядке (от новых к старым),
		// для вывода на экран их нужно перевернуть обратно
		for i := len(history) - 1; i >= 0; i-- {
			msg := history[i]
			formattedHistory := fmt.Sprintf("%s: %s\n", msg.Sender, msg.Text)
			_, _ = client.conn.Write([]byte(formattedHistory))
		}
		_, _ = client.conn.Write([]byte("—— Выше история сообщений ——\n\n"))
	}

	// Оповещаем остальных в комнате о новом участнике
	h.Broadcast(roomName, nil, fmt.Sprintf("— %s присоединился к комнате %s —\n", client.name, roomName))
}

// Broadcast отправляет сообщение всем участникам комнаты, кроме автора
func (h *Hub) Broadcast(roomName string, sender *Client, message string) {
	if roomName == "" {
		return
	}

	h.mu.Lock()
	clients := h.roomClients(roomName)
	h.mu.Unlock()

	if len(clients) == 0 {
		return
	}

	formattedMessage := message
	if sender != nil {
		formattedMessage = fmt.Sprintf("%s: %s\n", sender.name, message)
		h.saveMessage(roomName, sender.name, message)
	}

	h.writeToClients(clients, formattedMessage)
}

// GetRoomsWithCounts возвращает строку со списком комнат и количеством людей в них
func (h *Hub) GetRoomsWithCounts() string {
	// 1. Дефолтные комнаты, которые всегда должны быть доступны
	defaultRooms := []string{"general", "crypto", "random", "gamedev"}

	// Карта для объединения комнат и исключения дубликатов
	uniqueRooms := make(map[string]bool)
	for _, r := range defaultRooms {
		uniqueRooms[r] = true
	}

	// 2. Достаем список всех уникальных комнат, которые уже созданы в базе данных
	var dbRooms []string
	// SQL-эквивалент: SELECT DISTINCT room FROM messages
	if database.DB != nil {
		err := database.DB.Model(&database.MessageDB{}).Distinct("room").Pluck("room", &dbRooms).Error
		if err == nil {
			for _, r := range dbRooms {
				if r != "" {
					uniqueRooms[r] = true
				}
			}
		} else {
			log.Printf("[DB ERROR] Не удалось получить список комнат: %v", err)
		}
	}

	// 3. Блокируем хаб мьютексом, чтобы безопасно посчитать текущий онлайн в комнатах
	h.mu.Lock()
	for roomName := range h.rooms {
		uniqueRooms[roomName] = true
	}

	// Сортируем список комнат по алфавиту для стабильного отображения
	var roomNames []string
	for roomName := range uniqueRooms {
		roomNames = append(roomNames, roomName)
	}
	sort.Strings(roomNames)

	var parts []string
	for _, roomName := range roomNames {
		onlineCount := 0
		if clients, exists := h.rooms[roomName]; exists {
			onlineCount = len(clients)
		}
		parts = append(parts, fmt.Sprintf("%s:%d", roomName, onlineCount))
	}
	h.mu.Unlock()

	// Возвращаем строку формата "ROOMS_LIST:general:2,crypto:0,my_secret_room:1"
	return "ROOMS_LIST:" + strings.Join(parts, ",")
}

func (h *Hub) roomClients(roomName string) []*Client {
	clients, exists := h.rooms[roomName]
	if !exists {
		return nil
	}

	roomClients := make([]*Client, 0, len(clients))
	for client := range clients {
		roomClients = append(roomClients, client)
	}
	return roomClients
}

func (h *Hub) saveMessage(roomName, senderName, message string) {
	if database.DB == nil {
		return
	}

	dbMsg := database.MessageDB{
		Room:   roomName,
		Sender: senderName,
		Text:   message,
	}
	if err := database.DB.Create(&dbMsg).Error; err != nil {
		log.Printf("[DB ERROR] Не удалось сохранить сообщение: %v", err)
	}
}

func (h *Hub) writeToClients(clients []*Client, message string) {
	for _, client := range clients {
		_, err := client.conn.Write([]byte(message))
		if err != nil {
			h.RemoveClient(client)
		}
	}
}

// RemoveClient полностью удаляет клиента из чата при отключении
func (h *Hub) RemoveClient(client *Client) {
	h.mu.Lock()
	leftRoom, leftRoomClients := h.leaveRoomInternal(client)
	h.mu.Unlock()

	if leftRoom != "" {
		h.writeToClients(leftRoomClients, fmt.Sprintf("— %s покинул комнату —\n", client.name))
	}
}

// Внутренний метод выхода из комнаты
func (h *Hub) leaveRoomInternal(client *Client) (string, []*Client) {
	if client.room == "" {
		return "", nil
	}

	roomName := client.room
	var clientsToNotify []*Client
	if clients, exists := h.rooms[client.room]; exists {
		delete(clients, client) // Удаляем из карты
		clientsToNotify = make([]*Client, 0, len(clients))
		for roomClient := range clients {
			clientsToNotify = append(clientsToNotify, roomClient)
		}

		// Если комната стала пустой, удаляем саму комнату для экономии памяти
		if len(clients) == 0 {
			delete(h.rooms, client.room)
		}
	}
	client.room = ""
	return roomName, clientsToNotify
}

// Вспомогательная функция очистки строк
func cleanString(raw string) string {
	// 1. ОГРАНИЧЕНИЕ ПО ДЛИНЕ (Защита от DOS/OOM атак)
	// Ограничиваем сообщение, например, в 500 символов (рун), чтобы не забить память.
	const maxRunes = 500

	var clean strings.Builder
	runeCount := 0

	for _, r := range raw {
		if runeCount >= maxRunes {
			break
		}

		// 2. ФИЛЬТРАЦИЯ НЕПЕЧАТНЫХ СИМВОЛОВ (Защита от ANSI-инъекций)
		if unicode.IsPrint(r) {
			clean.WriteRune(r)
			runeCount++
		}
	}

	return strings.TrimSpace(clean.String())
}
