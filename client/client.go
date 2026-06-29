package client

import (
	"bufio"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/atotto/clipboard"
)

// Run запускает клиент чата и подключается к указанному адресу
func Run(address string) {
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

	var savedNick string
	savedNick, err = LoadConfig()
	if err != nil || savedNick == "" {
		_ = SaveConfig("Anonymouse")
		savedNick = "Anonymouse"
		log.Printf("[WARNING] Не удалось прочитать конфиг: %v", err)
	}

	// Создаем программу Bubble Tea
	p := tea.NewProgram(initialModel(conn, savedNick))

	// Запускаем фоновую горутину для прослушивания сервера
	go listenServer(conn, p)

	// Запускаем интерфейс (блокирующий вызов)
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

type UpdateLocalConfigMsg struct {
	NewNick string
}

// getConfigFilepath возвращает путь к файлу конфига в зависимости от ОС
func getConfigFilepath() (string, error) {
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

type screen int

const (
	screenRooms screen = iota // Экран выбора комнат
	screenChat                // Экран активного чата
)

type serverMessageMsg string
type networkErrorMsg error

type model struct {
	viewport viewport.Model
	textarea textarea.Model
	conn     net.Conn

	activeScreen screen
	connected    bool
	currentRoom  string
	myName       string

	rooms        []roomInfo
	selectedRoom int
	roomOffset   int

	lastScrollTime time.Time

	messages             []string
	selectedMessageIndex int
	focusOnHistory       bool

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
			_, err := fmt.Fprintln(conn, text+"\n")
			if err != nil {
				return networkErrorMsg(err)
			}
		}
		return nil
	}
}

type disconnectMsg struct{}

// listenServer бесконечно слушает TLS-сервер
func listenServer(conn net.Conn, p *tea.Program) {
	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		text := scanner.Text()

		// ПРОВЕРКА: Если строка начинается со скрытого маркера от сервера
		if newNick, ok := strings.CutPrefix(text, "CMD:NICK_UPDATED:"); ok {
			p.Send(UpdateLocalConfigMsg{NewNick: newNick})
			continue
		}

		p.Send(serverMessageMsg(text))
	}
	if err := scanner.Err(); err != nil {
		p.Send(networkErrorMsg(err))
	} else {
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
		statusDot:   lipgloss.NewStyle().Foreground(lipgloss.Color("#FFA500")),
		myNick:      lipgloss.NewStyle().Foreground(lipgloss.Color("#FF69B4")).Bold(true),
		otherNick:   lipgloss.NewStyle().Foreground(lipgloss.Color("5")).Bold(true),
		selectedMsg: lipgloss.NewStyle().Background(lipgloss.Color("#333333")).Foreground(lipgloss.Color("#FFF")),
		normalMsg:   lipgloss.NewStyle(),
	}

	return model{
		viewport:             vp,
		textarea:             ta,
		conn:                 conn,
		activeScreen:         screenRooms,
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
		m.connected = false
		m.messages = append(m.messages, "⚠️ Соединение закрыто сервером.")
		m.updateViewportContent()
		return m, nil

	case networkErrorMsg:
		m.connected = false
		errMsg := fmt.Sprintf("❌ Сетевая ошибка: %v", msg)
		m.messages = append(m.messages, errMsg)
		m.updateViewportContent()
		return m, nil

	case UpdateLocalConfigMsg:
		m.myName = msg.NewNick
		_ = SaveConfig(msg.NewNick)
		return m, nil

	case tea.WindowSizeMsg:
		m.viewport.SetWidth(msg.Width)
		m.textarea.SetWidth(msg.Width)
		m.viewport.SetHeight(msg.Height - 6)

		if m.activeScreen == screenRooms {
			m.clampRoomView(m.viewport.Height())
		} else {
			m.updateViewportContent()
		}
		return m, nil

	case tea.MouseWheelMsg:
		if time.Since(m.lastScrollTime) < 60*time.Millisecond {
			return m, nil
		}
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
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}

		if m.activeScreen == screenRooms {
			switch msg.String() {
			case "up", "k":
				if m.selectedRoom > 0 {
					m.selectedRoom--
					m.clampRoomView(m.viewport.Height())
				}
			case "down", "j":
				if m.selectedRoom < len(m.rooms)-1 {
					m.selectedRoom++
					m.clampRoomView(m.viewport.Height())
				}
			case "enter":
				m.currentRoom = m.rooms[m.selectedRoom].name
				m.activeScreen = screenChat
				m.messages = []string{}
				m.textarea.Focus()
				return m, sendMessageCmd(m.conn, "/join "+m.currentRoom)
			}
			return m, nil
		}

		if m.activeScreen == screenChat {
			if m.focusOnHistory {
				switch msg.String() {
				case "esc", "i":
					m.focusOnHistory = false
					m.textarea.Focus()
					m.selectedMessageIndex = -1
					m.updateViewportContent()
					m.viewport.GotoBottom()

				case "h", "backspace":
					m.activeScreen = screenRooms
					m.focusOnHistory = false
					m.selectedMessageIndex = -1
					m.currentRoom = ""

					return m, sendMessageCmd(m.conn, "/leave")

				case "up", "k":
					if m.selectedMessageIndex > 0 {
						m.selectedMessageIndex--
						m.updateViewportContent()

						if m.selectedMessageIndex < m.viewport.YOffset() {
							m.viewport.ScrollUp(1)
						}
					}

				case "down", "j":
					if m.selectedMessageIndex < len(m.messages)-1 {
						m.selectedMessageIndex++
						m.updateViewportContent()

						if m.selectedMessageIndex >= m.viewport.YOffset()+m.viewport.Height() {
							m.viewport.ScrollDown(1)
						}
					}

				case "y":
					if m.selectedMessageIndex >= 0 && m.selectedMessageIndex < len(m.messages) {
						rawMsg := m.messages[m.selectedMessageIndex]
						if parts := strings.SplitN(rawMsg, ":", 2); len(parts) == 2 {
							_ = clipboard.WriteAll(strings.TrimSpace(parts[1]))
						} else {
							_ = clipboard.WriteAll(rawMsg)
						}
					}
				}
				return m, nil
			}

			switch msg.String() {
			case "esc":
				m.focusOnHistory = true
				m.textarea.Blur()
				m.selectedMessageIndex = len(m.messages) - 1

				if m.selectedMessageIndex >= m.viewport.Height() {
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
				var cmd tea.Cmd
				m.textarea, cmd = m.textarea.Update(msg)
				return m, cmd
			}
		}

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
				_, _ = fmt.Sscanf(roomData[1], "%d", &count)
				updatedRooms = append(updatedRooms, roomInfo{
					name:      roomData[0],
					userCount: count,
				})
			}
			m.rooms = updatedRooms
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

	if m.activeScreen == screenChat && !m.focusOnHistory {
		var cmd tea.Cmd
		m.textarea, cmd = m.textarea.Update(msg)
		return m, cmd
	}

	return m, nil
}

func (m *model) updateViewportContent() {
	var renderedLines []string
	wrapWidth := max(m.viewport.Width()-2, 10)

	for i, rawLine := range m.messages {
		var formattedLine string
		prefix := m.myName + ":"

		if text, ok := strings.CutPrefix(rawLine, prefix); ok {
			formattedLine = m.styles.myNick.Render("Вы:") + text
		} else if parts := strings.SplitN(rawLine, ":", 2); len(parts) == 2 {
			formattedLine = m.styles.otherNick.Render(parts[0]+":") + parts[1]
		} else {
			formattedLine = lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Render(rawLine)
		}

		wrappedLine := lipgloss.NewStyle().Width(wrapWidth).Render(formattedLine)

		if m.focusOnHistory && i == m.selectedMessageIndex {
			subLines := strings.Split(wrappedLine, "\n")
			for j, subLine := range subLines {
				subLines[j] = m.styles.selectedMsg.Render(subLine)
			}
			wrappedLine = strings.Join(subLines, "\n")
		}

		renderedLines = append(renderedLines, wrappedLine)
	}

	m.viewport.SetContent(strings.Join(renderedLines, "\n"))
}

func (m model) View() tea.View {
	statusDot := "●"
	if m.connected {
		statusDot = m.styles.statusDot.Render("●")
	}

	headerText := fmt.Sprintf(" %s ", m.styles.header.Render("shreeemp"))
	if m.currentRoom != "" {
		headerText += fmt.Sprintf("  %s", m.styles.roomBadge.Render(m.currentRoom))
	}

	spaces := strings.Repeat(" ", max(1, m.viewport.Width()-lipgloss.Width(headerText)-3))
	headerPanel := headerText + spaces + statusDot + "\n" + strings.Repeat("─", m.viewport.Width()) + "\n"

	var body string
	if m.activeScreen == screenRooms {
		body = " ВЫБЕРИТЕ КАНАЛ ДЛЯ ВХОДА:\n\n"

		if len(m.rooms) == 0 {
			body += " Загрузка списка комнат от сервера...\n"
		}

		maxVisible := max(m.viewport.Height()-4, 2)
		endIdx := min(m.roomOffset+maxVisible+10, len(m.rooms))

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
		navigationHint := ""
		if m.focusOnHistory {
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
