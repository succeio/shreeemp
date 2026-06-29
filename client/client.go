package client

import (
	"bufio"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	"github.com/atotto/clipboard"
)

// Run запускает клиент чата
func Run(address string) {
	var savedNick string
	var err error
	savedNick, err = LoadConfig()
	if err != nil || savedNick == "" {
		savedNick = "Anonymouse"
	}

	nickname, serverAddr, theme, ok := runSettingsForm(savedNick, address)
	if !ok {
		fmt.Println("Настройка отменена.")
		os.Exit(0)
	}

	// Сохраняем имя в конфиг
	_ = SaveConfig(nickname)

	// Создаем программу Bubble Tea
	p := tea.NewProgram(initialModel(serverAddr, nickname, theme))

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
		return "", nil
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

func runSettingsForm(defaultNick, defaultAddr string) (nickname, serverAddr, theme string, ok bool) {
	nickname = defaultNick
	serverAddr = defaultAddr
	theme = "Catppuccin Mocha"

	form := huh.NewForm(
		huh.NewGroup(
			huh.NewNote().
				Title("SHREEEMP CHAT").
				Description("Добро пожаловать в TUI-чат нового поколения!\nПожалуйста, заполните параметры подключения:"),

			huh.NewInput().
				Title("Никнейм").
				Value(&nickname).
				Placeholder("Введите ваш ник...").
				Validate(func(str string) error {
					if len(strings.TrimSpace(str)) == 0 {
						return fmt.Errorf("никнейм не должен быть пустым")
					}
					return nil
				}),

			huh.NewInput().
				Title("Адрес сервера").
				Value(&serverAddr).
				Placeholder("localhost:8443").
				Validate(func(str string) error {
					if len(strings.TrimSpace(str)) == 0 {
						return fmt.Errorf("адрес не должен быть пустым")
					}
					return nil
				}),

			huh.NewSelect[string]().
				Title("Тема оформления").
				Options(
					huh.NewOption("Catppuccin Mocha (Темная)", "Catppuccin Mocha"),
					huh.NewOption("Nord (Снежная)", "Nord"),
					huh.NewOption("Dracula (Высококонтрастная)", "Dracula"),
					huh.NewOption("Everforest Medium (Лесная)", "Everforest Medium"),
				).
				Value(&theme),
		),
	)

	err := form.Run()
	if err != nil {
		return "", "", "", false
	}
	return nickname, serverAddr, theme, true
}

type focusState int

const (
	focusRooms focusState = iota
	focusInput
	focusHistory
)

type appState int

const (
	stateConnecting appState = iota
	stateError
	stateChat
)

type serverMessageMsg string
type networkErrorMsg error

type connectResultMsg struct {
	conn net.Conn
	err  error
}

type readMessageResult struct {
	text string
	err  error
}

type copyFlashTimeoutMsg struct{}
type clearNotificationMsg struct{}
type roomsTickMsg struct{}

type roomInfo struct {
	name      string
	userCount int
}

type messageLineRange struct {
	startLine int
	endLine   int
}

type model struct {
	viewport viewport.Model
	textarea textarea.Model
	conn     net.Conn

	state      appState
	spinner    spinner.Model
	serverAddr string
	themeName  string

	focus       focusState
	connected   bool
	currentRoom string
	myName      string

	rooms        []roomInfo
	selectedRoom int
	roomOffset   int

	lastScrollTime time.Time

	messages             []string
	messageRanges        []messageLineRange
	selectedMessageIndex int

	copyFlashActive bool
	notification    string
	colorCycle      int

	width  int
	height int

	// Размеры панелей
	sidebarWidth int
	chatWidth    int
	panelHeight  int

	styles uiStyles
}

var gradientColors = [][3]int{
	{203, 166, 247}, // Mauve
	{245, 194, 231}, // Pink
	{243, 139, 168}, // Red
	{250, 179, 135}, // Peach
	{249, 226, 175}, // Yellow
	{166, 227, 161}, // Green
	{148, 226, 213}, // Teal
	{137, 180, 250}, // Blue
	{180, 190, 254}, // Lavender
}

func makeGradient(text string, color1, color2 [3]int) string {
	runes := []rune(text)
	n := len(runes)
	if n == 0 {
		return ""
	}
	if n == 1 {
		cHex := fmt.Sprintf("#%02x%02x%02x", color1[0], color1[1], color1[2])
		return lipgloss.NewStyle().Foreground(lipgloss.Color(cHex)).Bold(true).Render(text)
	}

	var sb strings.Builder
	for i, r := range runes {
		t := float64(i) / float64(n-1)
		rVal := int(float64(color1[0]) + t*float64(color2[0]-color1[0]))
		gVal := int(float64(color1[1]) + t*float64(color2[1]-color1[1]))
		bVal := int(float64(color1[2]) + t*float64(color2[2]-color1[2]))
		cHex := fmt.Sprintf("#%02x%02x%02x", rVal, gVal, bVal)
		sb.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color(cHex)).Bold(true).Render(string(r)))
	}
	return sb.String()
}

func (m model) getTitleGradient() string {
	c1 := gradientColors[m.colorCycle%len(gradientColors)]
	c2 := gradientColors[(m.colorCycle+3)%len(gradientColors)]
	return makeGradient("SHREEEMP", c1, c2)
}

// Стили для интерфейса
type uiStyles struct {
	headerPanel lipgloss.Style
	appTitle    lipgloss.Style
	statusDot   lipgloss.Style
	myNick      lipgloss.Style
	otherNick   lipgloss.Style
	systemMsg   lipgloss.Style
	selectedMsg lipgloss.Style
	normalMsg   lipgloss.Style

	sidebar      lipgloss.Style
	sidebarFocus lipgloss.Style
	chatPanel    lipgloss.Style
	chatFocus    lipgloss.Style

	sidebarTitle     lipgloss.Style
	roomNormal       lipgloss.Style
	roomSelected     lipgloss.Style
	roomActive       lipgloss.Style
	roomActiveSel    lipgloss.Style
	roomUserCount    lipgloss.Style
	roomUserCountSel lipgloss.Style

	chatHeader     lipgloss.Style
	chatRoomName   lipgloss.Style
	inputArea      lipgloss.Style
	inputAreaFocus lipgloss.Style

	helpBar  lipgloss.Style
	helpKey  lipgloss.Style
	helpDesc lipgloss.Style
}

func getThemeStyles(themeName string) uiStyles {
	var text, subtext, surface0, overlay0, mauve, lavender, green, yellow, sky, pink string

	switch themeName {
	case "Nord":
		text = "#d8dee9"
		subtext = "#4c566a"
		surface0 = "#2e3440"
		overlay0 = "#434c5e"
		mauve = "#88c0d0"
		lavender = "#81a1c1"
		green = "#a3be8c"
		yellow = "#ebcb8b"
		sky = "#8fbcbb"
		pink = "#b48ead"

	case "Dracula":
		text = "#f8f8f2"
		subtext = "#6272a4"
		surface0 = "#282a36"
		overlay0 = "#44475a"
		mauve = "#bd93f9"
		lavender = "#8be9fd"
		green = "#50fa7b"
		yellow = "#f1fa8c"
		sky = "#ff79c6"
		pink = "#ff5555"

	case "Everforest Medium":
		text = "#d3c6aa"
		subtext = "#859289"
		surface0 = "#2d353b"
		overlay0 = "#3d484d"
		mauve = "#e67e80"
		lavender = "#83c092"
		green = "#a7c080"
		yellow = "#dbbc7f"
		sky = "#7fbbb3"
		pink = "#e67e80"

	default: // "Catppuccin Mocha"
		text = "#cdd6f4"
		subtext = "#a6adc8"
		surface0 = "#313244"
		overlay0 = "#585b70"
		mauve = "#cba6f7"
		lavender = "#b4befe"
		green = "#a6e3a1"
		yellow = "#f9e2af"
		sky = "#89dceb"
		pink = "#f5c2e7"
	}

	return uiStyles{
		headerPanel: lipgloss.NewStyle().
			Background(lipgloss.Color(surface0)).
			Foreground(lipgloss.Color(text)).
			Padding(0, 1).
			Height(1),
		appTitle: lipgloss.NewStyle().
			Foreground(lipgloss.Color(mauve)).
			Bold(true),
		statusDot: lipgloss.NewStyle().
			Foreground(lipgloss.Color(green)),
		myNick: lipgloss.NewStyle().
			Foreground(lipgloss.Color(pink)).
			Bold(true),
		otherNick: lipgloss.NewStyle().
			Foreground(lipgloss.Color(sky)).
			Bold(true),
		systemMsg: lipgloss.NewStyle().
			Foreground(lipgloss.Color(subtext)).
			Italic(true),
		selectedMsg: lipgloss.NewStyle().
			Background(lipgloss.Color(overlay0)).
			Foreground(lipgloss.Color(pink)).
			Bold(true),
		normalMsg: lipgloss.NewStyle().
			Foreground(lipgloss.Color(text)),

		sidebar: lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color(overlay0)).
			Padding(0, 1),
		sidebarFocus: lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color(mauve)).
			Padding(0, 1),
		chatPanel: lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color(overlay0)).
			Padding(0, 1),
		chatFocus: lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color(mauve)).
			Padding(0, 1),

		sidebarTitle: lipgloss.NewStyle().
			Foreground(lipgloss.Color(lavender)).
			Bold(true).
			Underline(true).
			MarginBottom(1),
		roomNormal: lipgloss.NewStyle().
			Foreground(lipgloss.Color(text)),
		roomSelected: lipgloss.NewStyle().
			Foreground(lipgloss.Color(mauve)).
			Bold(true),
		roomActive: lipgloss.NewStyle().
			Foreground(lipgloss.Color(green)).
			Bold(true),
		roomActiveSel: lipgloss.NewStyle().
			Foreground(lipgloss.Color(green)).
			Background(lipgloss.Color(surface0)).
			Bold(true),
		roomUserCount: lipgloss.NewStyle().
			Foreground(lipgloss.Color(subtext)),
		roomUserCountSel: lipgloss.NewStyle().
			Foreground(lipgloss.Color(yellow)),

		chatHeader: lipgloss.NewStyle().
			Border(lipgloss.NormalBorder(), false, false, true, false).
			BorderForeground(lipgloss.Color(overlay0)).
			PaddingBottom(0).
			MarginBottom(1),
		chatRoomName: lipgloss.NewStyle().
			Foreground(lipgloss.Color(lavender)).
			Bold(true),
		inputArea: lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color(overlay0)).
			Padding(0, 1),
		inputAreaFocus: lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color(mauve)).
			Padding(0, 1),

		helpBar: lipgloss.NewStyle().
			Background(lipgloss.Color(surface0)).
			Foreground(lipgloss.Color(subtext)).
			Padding(0, 1).
			Height(1),
		helpKey: lipgloss.NewStyle().
			Foreground(lipgloss.Color(mauve)).
			Bold(true),
		helpDesc: lipgloss.NewStyle().
			Foreground(lipgloss.Color(text)),
	}
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

func connectServerCmd(address string, nickname string) tea.Cmd {
	return func() tea.Msg {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			host = address
		}

		var conn net.Conn

		if host == "localhost" || host == "127.0.0.1" {
			conn, err = net.DialTimeout("tcp", address, 5*time.Second)
		} else {
			tlsConfig := &tls.Config{
				MinVersion: tls.VersionTLS13,
				ServerName: host,
			}
			dialer := &net.Dialer{Timeout: 5 * time.Second}
			conn, err = tls.DialWithDialer(dialer, "tcp", address, tlsConfig)
		}

		return connectResultMsg{conn: conn, err: err}
	}
}

func readMessageCmd(conn net.Conn) tea.Cmd {
	return func() tea.Msg {
		if conn == nil {
			return nil
		}
		reader := bufio.NewReader(conn)
		text, err := reader.ReadString('\n')
		if err != nil {
			return readMessageResult{err: err}
		}
		return readMessageResult{text: strings.TrimSuffix(text, "\n")}
	}
}

func roomsTickCmd() tea.Cmd {
	return tea.Tick(15*time.Second, func(t time.Time) tea.Msg {
		return roomsTickMsg{}
	})
}

func initialModel(serverAddr, nickname, themeName string) model {
	ta := textarea.New()
	ta.Placeholder = "Напишите сообщение..."
	ta.Focus()
	ta.Prompt = "┃ "
	ta.SetHeight(1)

	s := ta.Styles()
	s.Focused.CursorLine = lipgloss.NewStyle()
	ta.SetStyles(s)

	vp := viewport.New(viewport.WithWidth(30), viewport.WithHeight(5))

	styles := getThemeStyles(themeName)

	var spinColor string
	switch themeName {
	case "Nord":
		spinColor = "#88c0d0"
	case "Dracula":
		spinColor = "#bd93f9"
	case "Everforest Medium":
		spinColor = "#a7c080"
	default:
		spinColor = "#cba6f7"
	}

	spin := spinner.New()
	spin.Spinner = spinner.Dot
	spin.Style = lipgloss.NewStyle().Foreground(lipgloss.Color(spinColor))

	m := model{
		viewport:             vp,
		textarea:             ta,
		state:                stateConnecting,
		spinner:              spin,
		serverAddr:           serverAddr,
		themeName:            themeName,
		connected:            false,
		rooms:                []roomInfo{},
		selectedRoom:         0,
		messages:             []string{},
		messageRanges:        []messageLineRange{},
		selectedMessageIndex: -1,
		myName:               nickname,
		styles:               styles,
		width:                80,
		height:               24,
	}
	m.recalcLayout()
	return m
}

func (m model) Init() tea.Cmd {
	return tea.Batch(m.spinner.Tick, connectServerCmd(m.serverAddr, m.myName), roomsTickCmd())
}

func (m model) isFocused(f focusState) bool {
	if m.currentRoom == "" {
		return f == focusRooms
	}
	return m.focus == f
}

func (m *model) nextFocus() {
	if m.currentRoom == "" {
		m.focus = focusRooms
		return
	}
	switch m.focus {
	case focusRooms:
		m.focus = focusInput
		m.textarea.Focus()
	case focusInput:
		m.focus = focusHistory
		m.textarea.Blur()
		if len(m.messages) > 0 {
			m.selectedMessageIndex = len(m.messages) - 1
		} else {
			m.selectedMessageIndex = -1
		}
		m.updateViewportContent()
		m.scrollSelectionIntoView()
	case focusHistory:
		m.focus = focusRooms
		m.textarea.Blur()
		m.selectedMessageIndex = -1
		m.updateViewportContent()
	}
}

func (m *model) prevFocus() {
	if m.currentRoom == "" {
		m.focus = focusRooms
		return
	}
	switch m.focus {
	case focusRooms:
		m.focus = focusHistory
		m.textarea.Blur()
		if len(m.messages) > 0 {
			m.selectedMessageIndex = len(m.messages) - 1
		} else {
			m.selectedMessageIndex = -1
		}
		m.updateViewportContent()
		m.scrollSelectionIntoView()
	case focusInput:
		m.focus = focusRooms
		m.textarea.Blur()
	case focusHistory:
		m.focus = focusInput
		m.textarea.Focus()
		m.selectedMessageIndex = -1
		m.updateViewportContent()
	}
}

func (m *model) scrollSelectionIntoView() {
	if m.selectedMessageIndex < 0 || m.selectedMessageIndex >= len(m.messageRanges) {
		return
	}
	r := m.messageRanges[m.selectedMessageIndex]
	vpHeight := m.viewport.Height()
	yOffset := m.viewport.YOffset()

	if r.startLine < yOffset {
		m.viewport.SetYOffset(r.startLine)
	} else if r.endLine >= yOffset+vpHeight {
		m.viewport.SetYOffset(r.endLine - vpHeight + 1)
	}
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd

	case connectResultMsg:
		if msg.err != nil {
			m.state = stateError
			m.connected = false
			return m, nil
		}
		m.state = stateChat
		m.connected = true
		m.conn = msg.conn
		m.recalcLayout()
		return m, tea.Batch(
			sendMessageCmd(m.conn, m.myName),
			readMessageCmd(m.conn),
		)

	case readMessageResult:
		if msg.err != nil {
			m.connected = false
			m.messages = append(m.messages, "⚠️ Соединение закрыто сервером.")
			m.updateViewportContent()
			return m, nil
		}

		rawLine := msg.text

		if f, err := os.OpenFile("/tmp/shreeemp_debug.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644); err == nil {
			fmt.Fprintf(f, "[CLIENT] Received: %q\n", rawLine)
			f.Close()
		}

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
				m.focus = focusRooms
			}
			m.clampRoomView()
			return m, readMessageCmd(m.conn)
		}

		if newNick, ok := strings.CutPrefix(rawLine, "CMD:NICK_UPDATED:"); ok {
			m.myName = newNick
			_ = SaveConfig(newNick)
			return m, readMessageCmd(m.conn)
		}

		m.messages = append(m.messages, rawLine)
		m.updateViewportContent()
		if !m.isFocused(focusHistory) {
			m.viewport.GotoBottom()
		}
		return m, readMessageCmd(m.conn)

	case copyFlashTimeoutMsg:
		m.copyFlashActive = false
		m.updateViewportContent()
		return m, nil

	case clearNotificationMsg:
		m.notification = ""
		return m, nil

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.recalcLayout()
		return m, nil

	case tea.MouseWheelMsg:
		if time.Since(m.lastScrollTime) < 60*time.Millisecond {
			return m, nil
		}
		m.lastScrollTime = time.Now()

		if m.isFocused(focusRooms) {
			switch msg.Button {
			case tea.MouseWheelUp:
				if m.selectedRoom > 0 {
					m.selectedRoom--
					m.clampRoomView()
				}
			case tea.MouseWheelDown:
				if m.selectedRoom < len(m.rooms)-1 {
					m.selectedRoom++
					m.clampRoomView()
				}
			}
			return m, nil
		}

		if m.currentRoom != "" {
			switch msg.Button {
			case tea.MouseWheelUp:
				m.viewport.ScrollUp(3)
			case tea.MouseWheelDown:
				m.viewport.ScrollDown(3)
			}
			return m, nil
		}

	case tea.KeyPressMsg:
		m.colorCycle++
		keyStr := msg.String()
		if keyStr == "ctrl+c" {
			return m, tea.Quit
		}

		if m.state == stateError {
			if keyStr == "r" || keyStr == "R" {
				m.state = stateConnecting
				return m, tea.Batch(m.spinner.Tick, connectServerCmd(m.serverAddr, m.myName))
			}
			return m, nil
		}

		// Tab / Shift+Tab для переключения фокуса
		if keyStr == "tab" {
			m.nextFocus()
			return m, nil
		}
		if keyStr == "shift+tab" {
			m.prevFocus()
			return m, nil
		}

		// Фокус на Списке комнат (Sidebar)
		if m.isFocused(focusRooms) {
			switch keyStr {
			case "up", "k", "л":
				if m.selectedRoom > 0 {
					m.selectedRoom--
					m.clampRoomView()
				}
			case "down", "j", "о":
				if m.selectedRoom < len(m.rooms)-1 {
					m.selectedRoom++
					m.clampRoomView()
				}
			case "enter", "l", "д":
				if len(m.rooms) > 0 && m.selectedRoom < len(m.rooms) {
					newRoom := m.rooms[m.selectedRoom].name
					if newRoom != m.currentRoom {
						m.currentRoom = newRoom
						m.messages = []string{}
						m.focus = focusInput
						m.textarea.Focus()
						m.recalcLayout()
						m.viewport.GotoBottom()
						return m, sendMessageCmd(m.conn, "/join "+newRoom)
					}
				}
			}
			return m, nil
		}

		// Фокус на Истории сообщений
		if m.isFocused(focusHistory) {
			switch keyStr {
			case "esc", "i", "ш":
				m.focus = focusInput
				m.textarea.Focus()
				m.selectedMessageIndex = -1
				m.updateViewportContent()
				m.viewport.GotoBottom()

			case "up", "k", "л":
				if m.selectedMessageIndex > 0 {
					m.selectedMessageIndex--
					m.updateViewportContent()
					m.scrollSelectionIntoView()
				}

			case "down", "j", "о":
				if m.selectedMessageIndex < len(m.messages)-1 {
					m.selectedMessageIndex++
					m.updateViewportContent()
					m.scrollSelectionIntoView()
				}

			case "y", "н":
				if m.selectedMessageIndex >= 0 && m.selectedMessageIndex < len(m.messages) {
					rawMsg := m.messages[m.selectedMessageIndex]
					var textToCopy string
					prefix := m.myName + ":"
					if text, ok := strings.CutPrefix(rawMsg, prefix); ok {
						textToCopy = "Вы: " + text
					} else {
						textToCopy = rawMsg
					}

					err := clipboard.WriteAll(textToCopy)
					if err != nil {
						m.notification = "✗ Ошибка копирования"
					} else {
						m.notification = "✓ Скопировано!"
					}
					m.copyFlashActive = true
					m.updateViewportContent()

					return m, tea.Batch(
						tea.Tick(150*time.Millisecond, func(t time.Time) tea.Msg {
							return copyFlashTimeoutMsg{}
						}),
						tea.Tick(2000*time.Millisecond, func(t time.Time) tea.Msg {
							return clearNotificationMsg{}
						}),
					)
				}

			case "p", "P", "з":
				clipText, err := clipboard.ReadAll()
				if err != nil {
					m.notification = "✗ Ошибка вставки"
				} else {
					currentVal := m.textarea.Value()
					if currentVal != "" && !strings.HasSuffix(currentVal, " ") {
						currentVal += " "
					}
					m.textarea.SetValue(currentVal + clipText)
					m.focus = focusInput
					m.textarea.Focus()
					m.selectedMessageIndex = -1
					m.updateViewportContent()
					m.viewport.GotoBottom()
					m.notification = "✓ Вставлено!"
				}

				return m, tea.Tick(2000*time.Millisecond, func(t time.Time) tea.Msg {
					return clearNotificationMsg{}
				})
			}
			return m, nil
		}

		// Фокус на вводе текста
		if m.isFocused(focusInput) {
			switch keyStr {
			case "esc":
				m.focus = focusHistory
				m.textarea.Blur()
				if len(m.messages) > 0 {
					m.selectedMessageIndex = len(m.messages) - 1
				} else {
					m.selectedMessageIndex = -1
				}
				m.updateViewportContent()
				m.scrollSelectionIntoView()
				return m, nil

			case "enter":
				text := strings.TrimSpace(m.textarea.Value())
				if text == "" {
					return m, nil
				}
				m.textarea.Reset()

				if text == "/leave" {
					m.currentRoom = ""
					m.focus = focusRooms
					m.textarea.Blur()
					m.messages = []string{}
					m.recalcLayout()
					return m, sendMessageCmd(m.conn, "/leave")
				}

				return m, sendMessageCmd(m.conn, text)

			default:
				var cmd tea.Cmd
				m.textarea, cmd = m.textarea.Update(msg)
				return m, cmd
			}
		}
	}

	return m, nil
}

func (m *model) recalcLayout() {
	m.sidebarWidth = 25
	m.chatWidth = m.width - m.sidebarWidth

	if m.chatWidth < 20 {
		m.sidebarWidth = 15
		m.chatWidth = m.width - m.sidebarWidth
		if m.chatWidth < 10 {
			m.chatWidth = 10
		}
	}

	m.panelHeight = m.height - 2
	if m.panelHeight < 5 {
		m.panelHeight = 5
	}

	chatInnerWidth := m.chatWidth - 4
	chatInnerHeight := m.panelHeight - 2

	viewportHeight := chatInnerHeight - 6
	if viewportHeight < 3 {
		viewportHeight = 3
	}

	m.viewport.SetWidth(chatInnerWidth)
	m.viewport.SetHeight(viewportHeight)

	m.textarea.SetWidth(chatInnerWidth - 4)

	m.clampRoomView()
	m.updateViewportContent()
}

func (m *model) updateViewportContent() {
	var renderedLines []string
	wrapWidth := max(m.viewport.Width()-2, 10)

	m.messageRanges = make([]messageLineRange, len(m.messages))
	currentLine := 0

	for i, rawLine := range m.messages {
		prefix := m.myName + ":"
		var wrappedLine string
		var subLines []string
		var lineCount int

		isSelected := m.isFocused(focusHistory) && i == m.selectedMessageIndex

		if isSelected {
			var textWithoutStyle string
			if text, ok := strings.CutPrefix(rawLine, prefix); ok {
				textWithoutStyle = "Вы:" + text
			} else {
				textWithoutStyle = rawLine
			}

			wrappedLine = lipgloss.NewStyle().Width(wrapWidth).Render(textWithoutStyle)
			subLines = strings.Split(wrappedLine, "\n")
			lineCount = len(subLines)

			var style lipgloss.Style
			if m.copyFlashActive {
				// Яркая зеленая вспышка при копировании
				style = lipgloss.NewStyle().
					Background(lipgloss.Color("#a6e3a1")).
					Foreground(lipgloss.Color("#11111b")).
					Bold(true)
			} else {
				style = m.styles.selectedMsg
			}

			for j, subLine := range subLines {
				subLines[j] = style.Width(wrapWidth).Render(subLine)
			}
			wrappedLine = strings.Join(subLines, "\n")
		} else {
			var formattedLine string
			if text, ok := strings.CutPrefix(rawLine, prefix); ok {
				formattedLine = m.styles.myNick.Render("Вы:") + text
			} else if parts := strings.SplitN(rawLine, ":", 2); len(parts) == 2 {
				formattedLine = m.styles.otherNick.Render(parts[0]+":") + parts[1]
			} else if strings.HasPrefix(rawLine, "— ") || strings.HasPrefix(rawLine, "⚠️") || strings.HasPrefix(rawLine, "❌") {
				formattedLine = m.styles.systemMsg.Render(rawLine)
			} else {
				formattedLine = m.styles.normalMsg.Render(rawLine)
			}
			wrappedLine = lipgloss.NewStyle().Width(wrapWidth).Render(formattedLine)
			subLines = strings.Split(wrappedLine, "\n")
			lineCount = len(subLines)
		}

		m.messageRanges[i] = messageLineRange{
			startLine: currentLine,
			endLine:   currentLine + lineCount - 1,
		}
		currentLine += lineCount

		renderedLines = append(renderedLines, wrappedLine)
	}

	m.viewport.SetContent(strings.Join(renderedLines, "\n"))
}

func (m model) View() tea.View {
	if m.state == stateConnecting {
		spinStr := m.spinner.View()
		content := fmt.Sprintf("\n\n   %s Подключение к серверу %s...\n\n", spinStr, m.styles.chatRoomName.Render(m.serverAddr))
		v := tea.NewView(m.styles.chatPanel.Width(m.width - 2).Height(m.panelHeight).Render(content))
		v.AltScreen = true
		return v
	}

	if m.state == stateError {
		errorTitle := lipgloss.NewStyle().Foreground(lipgloss.Color("#f38ba8")).Bold(true).Render("❌ Ошибка подключения")
		errorDesc := m.styles.systemMsg.Render(fmt.Sprintf("Не удалось установить соединение с сервером %s.\n\nНажмите R для повторной попытки или Ctrl+C для выхода.", m.serverAddr))
		content := fmt.Sprintf("\n\n   %s\n\n   %s\n\n", errorTitle, errorDesc)
		v := tea.NewView(m.styles.chatPanel.Width(m.width - 2).Height(m.panelHeight).Render(content))
		v.AltScreen = true
		return v
	}

	statusDot := "●"
	if m.connected {
		statusDot = m.styles.statusDot.Render("●")
	}

	titleText := m.getTitleGradient()
	headerText := fmt.Sprintf(" %s %s", titleText, statusDot)

	var userText string
	if m.myName != "" {
		userText = fmt.Sprintf("Пользователь: %s [%s]", m.styles.myNick.Render(m.myName), m.themeName)
	}

	headerSpaces := max(1, m.width-lipgloss.Width(headerText)-lipgloss.Width(userText)-2)
	headerPanel := m.styles.headerPanel.Width(m.width).Render(
		headerText + strings.Repeat(" ", headerSpaces) + userText,
	)

	// Размеры панелей и элементов
	chatInnerWidth := m.chatWidth - 4
	chatInnerHeight := m.panelHeight - 2

	// Sidebar Content
	var sbBody strings.Builder
	sbBody.WriteString(m.styles.sidebarTitle.Render("Комнаты") + "\n")

	if len(m.rooms) == 0 {
		sbBody.WriteString(m.styles.systemMsg.Render("Загрузка...") + "\n")
	} else {
		maxVisibleRooms := max(chatInnerHeight-2, 2)
		endIdx := min(m.roomOffset+maxVisibleRooms, len(m.rooms))

		for i := m.roomOffset; i < endIdx; i++ {
			room := m.rooms[i]
			isCurrent := room.name == m.currentRoom
			isSelected := i == m.selectedRoom

			var style lipgloss.Style
			var badgeStyle lipgloss.Style

			if isCurrent && isSelected {
				style = m.styles.roomActiveSel
				badgeStyle = m.styles.roomUserCountSel
			} else if isCurrent {
				style = m.styles.roomActive
				badgeStyle = m.styles.roomUserCount
			} else if isSelected {
				style = m.styles.roomSelected
				badgeStyle = m.styles.roomUserCountSel
			} else {
				style = m.styles.roomNormal
				badgeStyle = m.styles.roomUserCount
			}

			prefix := "  "
			if isSelected {
				prefix = "> "
			}

			roomNameStr := room.name
			maxNameLen := m.sidebarWidth - 11
			if len(roomNameStr) > maxNameLen && maxNameLen > 3 {
				roomNameStr = roomNameStr[:maxNameLen-3] + "..."
			}

			roomLine := style.Render(prefix+roomNameStr) + badgeStyle.Render(fmt.Sprintf(" (👥%d)", room.userCount))
			sbBody.WriteString(roomLine + "\n")
		}
	}

	var sbStyle lipgloss.Style
	if m.isFocused(focusRooms) {
		sbStyle = m.styles.sidebarFocus
	} else {
		sbStyle = m.styles.sidebar
	}

	sidebarRendered := sbStyle.
		Width(m.sidebarWidth).
		Height(m.panelHeight).
		Render(sbBody.String())

	// Chat Panel Content
	var chatBody strings.Builder
	if m.currentRoom == "" {
		welcomeTitle := m.styles.chatRoomName.Render("Добро пожаловать в SHREEEMP!")
		welcomeDesc := m.styles.systemMsg.Render("Выберите комнату в списке слева для начала общения.\n\nИспользуйте клавиши ↑/↓ или j/k для перемещения по списку и Enter для входа.")
		chatBody.WriteString("\n\n" + welcomeTitle + "\n\n" + welcomeDesc)
	} else {
		roomHeader := m.styles.chatHeader.Width(chatInnerWidth).Render(
			m.styles.chatRoomName.Render("# "+m.currentRoom) + "  " +
				m.styles.systemMsg.Render("активный канал"),
		)
		chatBody.WriteString(roomHeader + "\n")
		chatBody.WriteString(m.viewport.View() + "\n")

		var inputStyle lipgloss.Style
		if m.isFocused(focusInput) {
			inputStyle = m.styles.inputAreaFocus
		} else {
			inputStyle = m.styles.inputArea
		}

		inputRendered := inputStyle.
			Width(chatInnerWidth).
			Render(m.textarea.View())

		chatBody.WriteString(inputRendered)
	}

	var cpStyle lipgloss.Style
	if m.isFocused(focusHistory) {
		cpStyle = m.styles.chatFocus
	} else {
		cpStyle = m.styles.chatPanel
	}

	chatRendered := cpStyle.
		Width(m.chatWidth).
		Height(m.panelHeight).
		Render(chatBody.String())

	// Footer Help Panel
	var helpParts []string
	addHelp := func(key, desc string) {
		helpParts = append(helpParts, m.styles.helpKey.Render(key)+" "+m.styles.helpDesc.Render(desc))
	}

	addHelp("Tab", "фокус")
	if m.isFocused(focusRooms) {
		addHelp("↑/↓/Enter", "выбрать")
	} else if m.isFocused(focusInput) {
		addHelp("Enter", "отправить")
		addHelp("Esc", "история")
	} else if m.isFocused(focusHistory) {
		addHelp("↑/↓/j/k", "скролл")
		addHelp("y", "копировать")
		addHelp("p", "вставить")
		addHelp("Esc/i/Enter", "писать")
	}
	addHelp("Ctrl+C", "выход")

	helpText := strings.Join(helpParts, "  •  ")

	var footerPanel string
	if m.notification != "" {
		notificationStyle := lipgloss.NewStyle().
			Foreground(lipgloss.Color("#11111b")).
			Background(lipgloss.Color("#a6e3a1")). // Зеленое уведомление для успеха
			Bold(true).
			Padding(0, 1)

		if strings.Contains(m.notification, "Ошибка") || strings.Contains(m.notification, "✗") {
			notificationStyle = notificationStyle.Background(lipgloss.Color("#f38ba8")) // Красное для ошибок
		}

		notificationText := notificationStyle.Render(m.notification)
		spacesCount := max(1, m.width-lipgloss.Width(helpText)-lipgloss.Width(notificationText)-4)
		footerPanel = m.styles.helpBar.Width(m.width).Render(
			helpText + strings.Repeat(" ", spacesCount) + notificationText,
		)
	} else {
		footerPanel = m.styles.helpBar.Width(m.width).Render(helpText)
	}

	mainLayout := lipgloss.JoinHorizontal(lipgloss.Top, sidebarRendered, chatRendered)

	v := tea.NewView(headerPanel + "\n" + mainLayout + "\n" + footerPanel)
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion

	return v
}

func (m *model) clampRoomView() {
	chatInnerHeight := m.panelHeight - 2
	maxVisible := max(chatInnerHeight-2, 2)

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
