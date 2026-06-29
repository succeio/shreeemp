package main

import (
	"flag"
	"fmt"
	"os"

	"shreeemp/client"
	"shreeemp/server"
)

func main() {
	// 1. Объявляем флаги
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

	address := fmt.Sprintf("%s:%d", *host, *port)

	// 4. Распределяем логику в зависимости от флага
	switch *mode {
	case "server":
		fmt.Printf("Запуск сервера на порту %d...\n", *port)
		server.Run(*port)
	case "client":
		fmt.Printf("Запуск клиента, подключение к %s...\n", address)
		client.Run(address)
	default:
		fmt.Printf("Неизвестный режим: %s. Используйте 'server' или 'client'.\n", *mode)
		os.Exit(1)
	}
}
