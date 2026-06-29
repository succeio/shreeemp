package database

import (
	"log"
	"os"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// DB - глобальная переменная для работы с БД
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

	// 2. Если переменная пустая, используем локальный дефолтный конфиг
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
		// которые остаются открытыми в памяти и ждут новых запросов
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

// CloseDB безопасно закрывает пул соединений с базой данных
func CloseDB() {
	if DB != nil {
		sqlDB, err := DB.DB()
		if err == nil {
			log.Println("[DB] Закрываем пул соединений с PostgreSQL...")
			sqlDB.Close()
		}
	}
}
