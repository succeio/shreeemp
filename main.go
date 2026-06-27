package main

import (
	"bufio"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/atotto/clipboard"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func main() {
	// 1. Объявляем флаги
	// Имя флага, значение по умолчанию, описание
	mode := flag.String("mode", "", "Режим работы: 'server' или 'client' (обязательный)")
	port := flag.Int("port", 8443, "Порт для подключения/прослушивания")
	host := flag.String("host", "localhost", "Адрес сервера (только для режима клиента)")

	// 2. Парсим аргументы, переданные при вызове
	flag.Parse()

	// 3. Проверяем, что пользователь ввел режим
	if *mode == "" {
		fmt.Println("Ошибка: необходимо указать флаг -mode")
		flag.Usage() // Выводит встроенную справку по флагам
		os.Exit(1)
	}

	// Обратите внимание: флаги возвращают УКАЗАТЕЛИ (*string, *int),
	// поэтому мы разыменовываем их через звездочку: *mode, *port
	address := fmt.Sprintf("%s:%d", *host, *port)

	// 4. Распределяем логику в зависимости от флага
	switch *mode {
	case "server":
		fmt.Printf("Запуск сервера на порту %d...\n", *port)
		runServer(*port)
	case "client":
		fmt.Printf("Запуск клиента, подключение к %s...\n", address)
		runClient(address)
	default:
		fmt.Printf("Неизвестный режим: %s. Используйте 'server' или 'client'.\n", *mode)
		os.Exit(1)
	}
}

// Глобальная переменная для работы с БД на сервере
var DB *gorm.DB

// MessageDB описывает структуру таблицы в PostgreSQL
type MessageDB struct {
	ID        uint      `gorm:"primaryKey"`
	Room      string    `gorm:"index;size:50;not null"` // Индекс для быстрого поиска по комнатам
	Sender    string    `gorm:"size:50;not null"`
	Text      string    `gorm:"type:text;not null"`
	CreatedAt time.Time `gorm:"index"` // Индекс для сортировки истории по времени
}

// TableName явно указывает имя таблицы в базе
func (MessageDB) TableName() string {
	return "messages"
}

// InitDB подключается к базе и создает таблицы, если их нет
func InitDB() {
	// Строка подключения (соответствует нашему docker-compose)
	// 1. Проверяем, передал ли Docker (или сервер) готовую строку подключения
	dsn := os.Getenv("DATABASE_URL")

	// 2. Если переменная пустая, используем ваш локальный дефолтный конфиг
	if dsn == "" {
		log.Println("[DB] Переменная DATABASE_URL не найдена, используем локальный хардкод...")
		dsn = "host=localhost user=chat_user password=chat_password dbname=chat_db port=5432 sslmode=disable TimeZone=Europe/Moscow"
	} else {
		log.Println("[DB] Успешно загружена конфигурация базы данных из окружения.")
	}
	var err error
	// Подключаемся с минимальным логированием (чтобы SQL-запросы не спамили в консоль сервера)
	DB, err = gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Error),
	})

	sqlDB, err := DB.DB()
	if err == nil {
		// Устанавливает максимальное количество ОТКРЫТЫХ соединений с БД.
		// Защищает Postgres от перегрузки, если у вас будет 10 000 горутин-клиентов.
		sqlDB.SetMaxOpenConns(100)

		// Устанавливает максимальное количество (idle) соединений в пуле,
		// которые остаются открытыми в памяти и ждут новых запросов (чтобы не тратить время на handshake с БД).
		sqlDB.SetMaxIdleConns(10)

		// Время, в течение которого соединение может быть переиспользовано, прежде чем закроется
		sqlDB.SetConnMaxLifetime(time.Hour)
	}

	if err != nil {
		log.Fatalf("[DB ERROR] Не удалось подключиться к базе данных: %v", err)
	}

	// 1. Автоматическое создание схем и таблиц
	err = DB.AutoMigrate(&MessageDB{})
	if err != nil {
		log.Fatalf("[DB ERROR] Ошибка миграции схемы: %v", err)
	}

	log.Println("[DB] Успешное подключение к PostgreSQL. Схемы проверены/созданы.")
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

		return tls.Listen("tcp", addr, config)
	}

	// СЦЕНАРИЙ Б: Сертификатов нет -> запускаем обычный TCP
	log.Printf("[SERVER WARNING] Режим разработки. Сертификаты не найдены. Запуск сервера на обычном TCP на %s...", addr)
	return net.Listen("tcp", addr)
}

// Сервер
func runServer(port int) {

	listener, err := createListener(port)
	if err != nil {
		log.Fatalf("[SERVER FATAL] Не удалось инициализировать сервер: %v", err)
	}
	defer listener.Close()

	InitDB()

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
	if DB != nil {
		sqlDB, err := DB.DB() // Извлекаем стандартный движок sql.DB из GORM
		if err == nil {
			log.Println("[SERVER] Закрываем пул соединений с PostgreSQL...")
			sqlDB.Close() // Безопасно закрывает все соединения в пуле
		}
	}

	log.Println("[SERVER] Сервер успешно завершил работу.")
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
		// Очищаем от мусора, как мы делали ранее
		nickname = cleanString(scanner.Text())
	}

	// Если клиент отключился сразу или прислал пустую строку
	if nickname == "" {
		nickname = "Anonymouse"
		// conn.Close()
		// return
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

	// Автоматически добавляем в общую комнату
	// hub.JoinRoom("general", client)

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

				// 2. ДОБАВЛЯЕМ: Отправляем скрытую команду клиенту.
				// Добавляем префикс CMD:NICK_UPDATED:, чтобы клиент опознал её.
				// Обязательно добавляем \n в конце, чтобы scanner.Scan() на клиенте её прочитал!
				cmdToClient := fmt.Sprintf("CMD:NICK_UPDATED:%s\n", newNick)
				client.conn.Write([]byte(cmdToClient)) // Используйте ваше имя переменной сокета (conn)

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

// Вспомогательная функция очистки строк (чтобы не дублировать код)
func cleanString(raw string) string {
	// 1. ОГРАНИЧЕНИЕ ПО ДЛИНЕ (Защита от DOS/OOM атак)
	// Ограничиваем сообщение, например, в 500 символов (рун), чтобы не забить память.
	const maxRunes = 500

	var clean strings.Builder
	runeCount := 0

	for _, r := range raw {
		if runeCount >= maxRunes {
			break // Прерываем чтение, если пользователь превысил лимит
		}

		// 2. ФИЛЬТРАЦИЯ НЕПЕЧАТНЫХ СИМВОЛОВ (Защита от ANSI-инъекций)
		if unicode.IsPrint(r) {
			clean.WriteRune(r)
			runeCount++
		}
	}

	return strings.TrimSpace(clean.String())
}

// Клиент
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

func (h *Hub) JoinRoom(roomName string, client *Client) {
	h.mu.Lock()
	if client.room != "" {
		h.leaveRoomInternal(client)
	}

	if _, exists := h.rooms[roomName]; !exists {
		h.rooms[roomName] = make(map[*Client]bool)
	}
	h.rooms[roomName][client] = true
	client.room = roomName
	h.mu.Unlock() // Разблокируем мьютекс, чтобы не забивать поток во время работы с БД

	// 2. ОТПРАВКА ИСТОРИИ ИЗ БД ДО ПРИВЕТСТВИЯ
	var history []MessageDB
	// Берем последние 50 сообщений, отсортированных по дате создания
	result := DB.Where("room = ?", roomName).Order("created_at desc").Limit(50).Find(&history)
	if result.Error == nil && len(history) > 0 {
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
	h.mu.Lock()
	h.broadcastInternal(roomName, nil, fmt.Sprintf("— %s присоединился к комнате %s —\n", client.name, roomName))
	h.mu.Unlock()
}

// Broadcast отправляет сообщение всем участникам комнаты, кроме автора
func (h *Hub) Broadcast(roomName string, sender *Client, message string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.broadcastInternal(roomName, sender, message)
}

// GetRoomsRoomsWithCounts возвращает строку со списком комнат и количеством людей в них
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
	err := DB.Model(&MessageDB{}).Distinct("room").Pluck("room", &dbRooms).Error
	if err == nil {
		for _, r := range dbRooms {
			if r != "" {
				uniqueRooms[r] = true
			}
		}
	}

	// 3. Блокируем хаб мьютексом, чтобы безопасно посчитать текущий онлайн в комнатах
	h.mu.Lock()
	var parts []string
	for roomName := range uniqueRooms {
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

func (h *Hub) broadcastInternal(roomName string, sender *Client, message string) {
	clients, exists := h.rooms[roomName]
	if !exists {
		return
	}

	formattedMessage := message
	if sender != nil {
		formattedMessage = fmt.Sprintf("%s: %s\n", sender.name, message)

		// СОХРАНЕНИЕ В БД: Записываем только сообщения от реальных пользователей (sender != nil)
		dbMsg := MessageDB{
			Room:   roomName,
			Sender: sender.name,
			Text:   message,
		}
		// Асинхронно или синхронно сохраняем в базу.
		// Метод .Create() сам заполнит ID и CreatedAt
		if err := DB.Create(&dbMsg).Error; err != nil {
			log.Printf("[DB ERROR] Не удалось сохранить сообщение: %v", err)
		}
	}

	for client := range clients {
		_, err := client.conn.Write([]byte(formattedMessage))
		if err != nil {
			go h.RemoveClient(client)
		}
	}
}

// RemoveClient полностью удаляет клиента из чата при отключении
func (h *Hub) RemoveClient(client *Client) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.leaveRoomInternal(client)
}

// Внутренний метод выхода из комнаты
func (h *Hub) leaveRoomInternal(client *Client) {
	if client.room == "" {
		return
	}

	if clients, exists := h.rooms[client.room]; exists {
		delete(clients, client) // Удаляем из карты
		h.broadcastInternal(client.room, nil, fmt.Sprintf("— %s покинул комнату —\n", client.name))

		// Если комната стала пустой, удаляем саму комнату для экономии памяти
		if len(clients) == 0 {
			delete(h.rooms, client.room)
		}
	}
	client.room = ""
}

// Старт клиента
func runClient(address string) {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		host = address
	}

	var conn net.Conn

	if host == "localhost" || host == "127.0.0.1" {
		log.Printf("[CLIENT] Подключение к локальной разработке по обычному TCP: %s...", address)
		// Для localhost используем самый простой net.Dial без TLS
		conn, err = net.Dial("tcp", address)
		if err != nil {
			log.Fatalf("[CLIENT ERROR] Не удалось подключиться к серверу по TCP: %v", err)
		}
	} else {
		log.Printf("[CLIENT] Подключение к рабочему серверу по безопасному TLS: %s...", address)
		// Для внешних серверов используем стандартный системный TLS
		tlsConfig := &tls.Config{
			MinVersion: tls.VersionTLS13,
			ServerName: host, // Защита от MITM-атак (проверка домена)
		}
		conn, err = tls.Dial("tcp", address, tlsConfig)
		if err != nil {
			log.Fatalf("[CLIENT ERROR] Сбой безопасного TLS подключения: %v", err)
		}
	}

	defer conn.Close()

	savedNick, err := LoadConfig()
	if err != nil {
		log.Printf("[WARNING] Не удалось прочитать конфиг: %v", err)
	}

	// 3. Создаем программу Bubble Tea
	p := tea.NewProgram(initialModel(conn, savedNick))

	// 4. Запускаем фоновую горутину для прослушивания сервера
	// Передаем туда объект программы `p`, чтобы горутина могла вызывать `p.Send()`
	go listenServer(conn, p)

	// 5. Запускаем интерфейс (блокирующий вызов)
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "[CLIENT ERROR] Ошибка TUI: %v\n", err)
		os.Exit(1)
	}
}

const appDirName = "shreeemp"
const configFileName = "config.json"

type Config struct {
	Username string `json:"username"`
}

type updateLocalConfigMsg struct {
	newNick string
}

// getConfigFilepath возвращает путь к файлу конфига в зависимости от ОС
func getConfigFilepath() (string, error) {
	// Находит %APPDATA% на Windows, ~/.config на Linux, ~/Library/Application Support на macOS
	baseDir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}

	appDir := filepath.Join(baseDir, appDirName)
	return filepath.Join(appDir, configFileName), nil
}

// LoadConfig читает ник из файла, если он существует
func LoadConfig() (string, error) {
	configPath, err := getConfigFilepath()
	if err != nil {
		return "", err
	}

	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		return "", nil // Файла нет, это нормально для первого запуска
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		return "", err
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return "", err
	}

	return cfg.Username, nil
}

// SaveConfig сохраняет ник на диск
func SaveConfig(username string) error {
	configPath, err := getConfigFilepath()
	if err != nil {
		return err
	}

	// Создаем папку приложения, если её ещё нет
	appDir := filepath.Dir(configPath)
	if err := os.MkdirAll(appDir, 0755); err != nil {
		return err
	}

	cfg := Config{Username: username}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(configPath, data, 0600)
}

// Bubble Tea
// Кастомное сообщение для Bubble Tea, когда сервер присылает текст
type screen int

const (
	screenRooms screen = iota // Экран выбора комнат
	screenChat                // Экран активного чата
)

type serverMessageMsg string

// Кастомное сообщение на случай обрыва связи
type networkErrorMsg error

type model struct {
	viewport viewport.Model
	textarea textarea.Model
	conn     net.Conn

	// Состояние интерфейса
	activeScreen screen
	connected    bool
	currentRoom  string
	myName       string

	// Навигация по комнатам
	rooms        []roomInfo
	selectedRoom int
	roomOffset   int

	lastScrollTime time.Time

	// Навигация по сообщениям (hjkl)
	messages             []string // Храним сырые сообщения от сервера
	selectedMessageIndex int      // Индекс сообщения, на котором стоит курсор навигации
	focusOnHistory       bool     // true, если пользователь нажал Esc и ходит по истории (hjkl)

	// Стили
	styles uiStyles
}

type roomInfo struct {
	name      string
	userCount int
}

type uiStyles struct {
	header      lipgloss.Style
	roomBadge   lipgloss.Style
	statusDot   lipgloss.Style
	myNick      lipgloss.Style
	otherNick   lipgloss.Style
	selectedMsg lipgloss.Style
	normalMsg   lipgloss.Style
}

// Команда для отправки текста на сервер в фоне
func sendMessageCmd(conn net.Conn, text string) tea.Cmd {
	return func() tea.Msg {
		if conn != nil {
			// Добавляем перенос строки, так как наш сервер читает через bufio.Scanner
			_, err := fmt.Fprintln(conn, text+"\n")
			if err != nil {
				return networkErrorMsg(err)
			}
		}
		return nil
	}
}

// disconnectMsg отправляется в Update, когда сокет закрывается
type disconnectMsg struct{}

// Функция-воркер, которая бесконечно слушает TLS-сервер
func listenServer(conn net.Conn, p *tea.Program) {
	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		text := scanner.Text()

		// ПРОВЕРКА: Если строка начинается со скрытого маркера от сервера
		if newNick, ok := strings.CutPrefix(text, "CMD:NICK_UPDATED:"); ok {
			p.Send(updateLocalConfigMsg{newNick: newNick})
			continue
		}

		// Если это не маркер, то это обычное сообщение чата, шлем как обычно
		p.Send(serverMessageMsg(text))
	}
	// Цикл завершился. Проверяем почему:
	if err := scanner.Err(); err != nil {
		// Отправляем сетевую ошибку, если она была
		p.Send(networkErrorMsg(err))
	} else {
		// Отправляем сигнал отключения, если это был чистый EOF (сервер лег)
		p.Send(disconnectMsg{})
	}
}

func initialModel(conn net.Conn, savedNick string) model {
	ta := textarea.New()
	ta.Placeholder = "Напишите сообщение..."
	ta.Focus()
	ta.Prompt = "┃ "
	ta.SetHeight(3)

	s := ta.Styles()
	s.Focused.CursorLine = lipgloss.NewStyle()
	ta.SetStyles(s)

	vp := viewport.New(viewport.WithWidth(30), viewport.WithHeight(5))

	styles := uiStyles{

		header:      lipgloss.NewStyle().Foreground(lipgloss.Color("#FFA500")).Bold(true),
		roomBadge:   lipgloss.NewStyle().Background(lipgloss.Color("#4CAF50")).Foreground(lipgloss.Color("#FFFFFF")).Padding(0, 1),
		statusDot:   lipgloss.NewStyle().Foreground(lipgloss.Color("#FFA500")), // Оранжевый круг
		myNick:      lipgloss.NewStyle().Foreground(lipgloss.Color("#FF69B4")).Bold(true),
		otherNick:   lipgloss.NewStyle().Foreground(lipgloss.Color("5")).Bold(true),
		selectedMsg: lipgloss.NewStyle().Background(lipgloss.Color("#333333")).Foreground(lipgloss.Color("#FFF")),
		normalMsg:   lipgloss.NewStyle(),
	}

	return model{
		viewport:             vp,
		textarea:             ta,
		conn:                 conn,
		activeScreen:         screenRooms, // Стартуем с выбора комнат
		connected:            conn != nil,
		rooms:                []roomInfo{},
		selectedRoom:         0,
		messages:             []string{},
		selectedMessageIndex: -1,
		focusOnHistory:       false,
		myName:               savedNick,
		styles:               styles,
	}
}

func (m model) Init() tea.Cmd {
	if m.myName != "" {
		return tea.Batch(textarea.Blink, sendMessageCmd(m.conn, m.myName))
	}
	return textarea.Blink
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case disconnectMsg:
		m.connected = false // Выключаем статус-дот
		m.messages = append(m.messages, "⚠️ Соединение закрыто сервером.")
		m.updateViewportContent()
		return m, nil

	case networkErrorMsg:
		m.connected = false // Выключаем статус-дот
		// Форматируем системную ошибку для вывода в чат
		errMsg := fmt.Sprintf("❌ Сетевая ошибка: %v", msg)
		m.messages = append(m.messages, errMsg)
		m.updateViewportContent()
		return m, nil

	case updateLocalConfigMsg:
		m.myName = msg.newNick      // Меняем в памяти UI
		_ = SaveConfig(msg.newNick) // Перезаписываем ваш config.json
		return m, nil

	case tea.WindowSizeMsg:
		m.viewport.SetWidth(msg.Width)
		m.textarea.SetWidth(msg.Width)
		m.viewport.SetHeight(msg.Height - 6)

		// Пересчитываем положение скользящего окна комнат под новый размер экрана
		if m.activeScreen == screenRooms {
			m.clampRoomView(m.viewport.Height())
		} else {
			m.updateViewportContent()
		}
		return m, nil

	case tea.MouseWheelMsg:
		// === Экран выбора комнат ===
		// Если с прошлого скролла прошло меньше 60мс — игнорируем пачечный сигнал терминала
		if time.Since(m.lastScrollTime) < 60*time.Millisecond {
			return m, nil
		}
		// Запоминаем время текущего скролла
		m.lastScrollTime = time.Now()
		if m.activeScreen == screenRooms {

			switch msg.Button {
			case tea.MouseWheelUp:
				if m.selectedRoom > 0 {
					m.selectedRoom--
					m.clampRoomView(m.viewport.Height())
				}
			case tea.MouseWheelDown:
				if m.selectedRoom < len(m.rooms)-1 {
					m.selectedRoom++
					m.clampRoomView(m.viewport.Height())
				}
			}
			return m, nil
		}

		// === Экран чата ===
		if m.activeScreen == screenChat && m.focusOnHistory {
			switch msg.Button {
			case tea.MouseWheelUp:
				if m.selectedMessageIndex > 0 {
					m.selectedMessageIndex--
					m.updateViewportContent()

					if m.selectedMessageIndex < m.viewport.YOffset() {
						m.viewport.ScrollUp(1)
					}
				}

			case tea.MouseWheelDown:
				if m.selectedMessageIndex < len(m.messages)-1 {
					m.selectedMessageIndex++
					m.updateViewportContent()

					if m.selectedMessageIndex >= m.viewport.YOffset()+m.viewport.Height() {
						m.viewport.ScrollDown(1)
					}
				}
			}
			return m, nil
		}

	case tea.KeyPressMsg:
		// Эти клавиши работают везде глобально
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}

		// === ЕСЛИ МЫ НА ЭКРАНЕ ВЫБОРА КОМНАТ ===
		if m.activeScreen == screenRooms {
			switch msg.String() {
			case "up", "k":
				if m.selectedRoom > 0 {
					m.selectedRoom--
					m.clampRoomView(m.viewport.Height()) // Добавили
				}
			case "down", "j":
				if m.selectedRoom < len(m.rooms)-1 {
					m.selectedRoom++
					m.clampRoomView(m.viewport.Height()) // Добавили
				}
			case "enter":
				// Внутри обработки enter на экране комнат:
				m.currentRoom = m.rooms[m.selectedRoom].name // Добавили .name
				m.activeScreen = screenChat
				m.messages = []string{}
				m.textarea.Focus()
				return m, sendMessageCmd(m.conn, "/join "+m.currentRoom)
			}
			return m, nil
		}

		// === ЕСЛИ МЫ ВНУТРИ ЧАТА ===
		if m.activeScreen == screenChat {
			// Если включен режим навигации по истории (Vim-mode)
			if m.focusOnHistory {
				switch msg.String() {
				case "esc", "i": // Нажатие 'i' или 'esc' возвращает в режим ввода
					m.focusOnHistory = false
					m.textarea.Focus()
					m.selectedMessageIndex = -1
					m.updateViewportContent()
					m.viewport.GotoBottom() // Сбрасываем скролл вниз

				case "h", "backspace":
					m.activeScreen = screenRooms
					m.focusOnHistory = false
					m.selectedMessageIndex = -1
					m.currentRoom = "" // Сбрасываем текущую комнату

					// Отправляем серверу команду выхода (например, /leave)
					// Сервер в ответ пришлет нам обновленный список комнат со свежими счетчиками!
					return m, sendMessageCmd(m.conn, "/leave")

				case "up", "k":
					if m.selectedMessageIndex > 0 {
						m.selectedMessageIndex--
						m.updateViewportContent()

						// Исправлено: YOffset() теперь метод.
						// Если курсор ушел выше верхнего края видимой области, скроллим вверх.
						if m.selectedMessageIndex < m.viewport.YOffset() {
							m.viewport.ScrollUp(1)
						}
					}

				case "down", "j":
					if m.selectedMessageIndex < len(m.messages)-1 {
						m.selectedMessageIndex++
						m.updateViewportContent()

						// Исправлено: YOffset() и Height() теперь методы, а LineDown заменен на ScrollDown.
						// Если курсор ушел ниже нижнего края видимой области, скроллим вниз.
						if m.selectedMessageIndex >= m.viewport.YOffset()+m.viewport.Height() {
							m.viewport.ScrollDown(1)
						}
					}

				case "y":
					if m.selectedMessageIndex >= 0 && m.selectedMessageIndex < len(m.messages) {
						rawMsg := m.messages[m.selectedMessageIndex]
						// Очищаем от ника перед копированием (копируем только текст)
						if parts := strings.SplitN(rawMsg, ":", 2); len(parts) == 2 {
							_ = clipboard.WriteAll(strings.TrimSpace(parts[1]))
						} else {
							_ = clipboard.WriteAll(rawMsg)
						}
					}
				}
				return m, nil
			}

			// Если мы в обычном режиме ввода текста
			switch msg.String() {
			case "esc": // Переход в режим навигации
				m.focusOnHistory = true
				m.textarea.Blur()
				m.selectedMessageIndex = len(m.messages) - 1

				// Исправлено: Height() теперь метод
				if m.selectedMessageIndex >= m.viewport.Height() {
					// Напрямую смещение в v2 задается через метод SetYOffset
					m.viewport.SetYOffset(m.selectedMessageIndex - m.viewport.Height() + 1)
				}
				m.updateViewportContent()
				return m, nil

			case "enter":
				text := strings.TrimSpace(m.textarea.Value())
				if text == "" {
					return m, nil
				}
				m.textarea.Reset()

				if text == "/leave" {
					m.activeScreen = screenRooms
					m.focusOnHistory = false
					m.selectedMessageIndex = -1
					m.currentRoom = ""
					return m, sendMessageCmd(m.conn, "/leave")
				}

				return m, sendMessageCmd(m.conn, text)

			default:
				// Все остальные клавиши (включая буквы i, j, k) безопасно отправляем в textarea
				var cmd tea.Cmd
				m.textarea, cmd = m.textarea.Update(msg)
				return m, cmd
			}
		}

	// Получение сообщений от сервера
	case serverMessageMsg:
		rawLine := string(msg)

		if data, ok := strings.CutPrefix(rawLine, "ROOMS_LIST:"); ok {
			parts := strings.Split(data, ",")
			var updatedRooms []roomInfo
			for _, part := range parts {
				roomData := strings.Split(part, ":")
				if len(roomData) != 2 {
					continue
				}
				count := 0
				fmt.Sscanf(roomData[1], "%d", &count)
				updatedRooms = append(updatedRooms, roomInfo{
					name:      roomData[0],
					userCount: count,
				})
			}
			m.rooms = updatedRooms
			// гарантируем, что активен экран выбора комнат
			if m.currentRoom == "" {
				m.activeScreen = screenRooms
			}
			return m, nil
		}

		m.messages = append(m.messages, rawLine)
		m.updateViewportContent()
		if !m.focusOnHistory {
			m.viewport.GotoBottom()
		}
		return m, nil
	}

	// Передаем ввод в textarea, только если мы не в режиме навигации hjkl
	if m.activeScreen == screenChat && !m.focusOnHistory {
		var cmd tea.Cmd
		m.textarea, cmd = m.textarea.Update(msg)
		return m, cmd
	}

	return m, nil
}

// Вспомогательный метод для обновления контента вьюпорта
func (m *model) updateViewportContent() {
	var renderedLines []string

	// Вычисляем доступную ширину для текста
	wrapWidth := max(m.viewport.Width()-2, 10)

	for i, rawLine := range m.messages {
		var formattedLine string
		prefix := m.myName + ":"

		// 1. Раскрашиваем никнеймы
		if text, ok := strings.CutPrefix(rawLine, prefix); ok {
			formattedLine = m.styles.myNick.Render("Вы:") + text
		} else if parts := strings.SplitN(rawLine, ":", 2); len(parts) == 2 {
			formattedLine = m.styles.otherNick.Render(parts[0]+":") + parts[1]
		} else {
			formattedLine = lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Render(rawLine)
		}

		// 2. Делаем автоматический перенос строки через создание стиля с фиксированной шириной.
		// Метод .Render() внутри себя сам сделает wrap под указанный Width.
		wrappedLine := lipgloss.NewStyle().Width(wrapWidth).Render(formattedLine)

		// 3. Подсвечиваем всю группу перенесенных строк, если на сообщении стоит курсор hjkl
		if m.focusOnHistory && i == m.selectedMessageIndex {
			subLines := strings.Split(wrappedLine, "\n")
			for j, subLine := range subLines {
				subLines[j] = m.styles.selectedMsg.Render(subLine)
			}
			wrappedLine = strings.Join(subLines, "\n")
		}

		renderedLines = append(renderedLines, wrappedLine)
	}

	// Отправляем перенесенный текст во вьюпорт
	m.viewport.SetContent(strings.Join(renderedLines, "\n"))
}

func (m model) View() tea.View {
	// 1. Собираем верхнюю панель (Header)
	statusDot := "●"
	if m.connected {
		statusDot = m.styles.statusDot.Render("●") // Оранжевый круг
	}

	headerText := fmt.Sprintf(" %s ", m.styles.header.Render("shreeemp"))
	if m.currentRoom != "" {
		headerText += fmt.Sprintf("  %s", m.styles.roomBadge.Render(m.currentRoom))
	}

	// Добавляем оранжевый индикатор в правый угол экрана
	spaces := strings.Repeat(" ", max(1, m.viewport.Width()-lipgloss.Width(headerText)-3))
	headerPanel := headerText + spaces + statusDot + "\n" + strings.Repeat("─", m.viewport.Width()) + "\n"

	// 2. Отрендеринг тела в зависимости от экрана
	var body string
	if m.activeScreen == screenRooms {
		body = " ВЫБЕРИТЕ КАНАЛ ДЛЯ ВХОДА:\n\n"

		if len(m.rooms) == 0 {
			body += " Загрузка списка комнат от сервера...\n"
		}

		// Вычисляем размер видимого окна с защитой от нуля и отрицательных чисел
		maxVisible := max(m.viewport.Height()-4, 2)

		endIdx := min(m.roomOffset+maxVisible+10, len(m.rooms))

		// Теперь этот цикл выполнится в любом случае
		for i := m.roomOffset; i < endIdx; i++ {
			room := m.rooms[i]
			cursor := "  "
			roomLine := fmt.Sprintf("%s (👥 %d)", room.name, room.userCount)

			if i == m.selectedRoom {
				cursor = m.styles.header.Render("> ")
				body += fmt.Sprintf("%s%s\n", cursor, lipgloss.NewStyle().Bold(true).Render(roomLine))
			} else {
				body += fmt.Sprintf("%s%s\n", cursor, roomLine)
			}
		}

	} else {
		// Экран чата
		navigationHint := ""
		if m.focusOnHistory {
			// Добавили упоминание 'h - назад к комнатам'
			navigationHint = lipgloss.NewStyle().Foreground(lipgloss.Color("#FFF")).Render(" [РЕЖИМ НАВИГАЦИИ: j/k - двигаться, y - копировать, h/Backspace  - к списку комнат, i - писать] ") + "\n"
		}
		body = navigationHint + m.viewport.View() + "\n" + m.textarea.View()
	}

	v := tea.NewView(headerPanel + body)
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion

	return v
}

func (m *model) clampRoomView(terminalHeight int) {
	// Вычитаем отступы под заголовок
	maxVisible := max(terminalHeight-4, 2)

	if len(m.rooms) <= maxVisible {
		m.roomOffset = 0
		return
	}

	if m.selectedRoom < m.roomOffset {
		m.roomOffset = m.selectedRoom
	}

	if m.selectedRoom >= m.roomOffset+maxVisible {
		m.roomOffset = m.selectedRoom - maxVisible + 1
	}
}
