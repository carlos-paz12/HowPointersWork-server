package main

import (
	"log"
	"net/http"

	"github.com/arturo32/HowPointersWork-server/handler"
	"github.com/runabol/tork/cli"
	"github.com/runabol/tork/conf"
	"github.com/runabol/tork/engine"
)

func main() {
	// Tenta carregar as configurações do sistema.
	// Se ocorrer erro, imprime e finaliza a aplicação.
	if err := conf.LoadConfig(); err != nil {
		log.Fatal(err)
	}

	// Registra o endpoint `/execute` para receber requisições POST
	// e redirecionar para o handler.
	engine.RegisterEndpoint(http.MethodPost, "/execute", handler.Handler)

	// Tenta iniciar o servidor web.
	// Se ocorrer erro, imprime e finaliza a aplicação.
	if err := cli.New().Run(); err != nil {
		log.Fatal(err)
	}
}
