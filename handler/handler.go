package handler

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/pkg/errors"
	"github.com/rs/zerolog/log"
	"github.com/runabol/tork"
	"github.com/runabol/tork/engine"
	"github.com/runabol/tork/input"
	"github.com/runabol/tork/middleware/web"
)

// ExecRequest representa o request de execução enviado pelo cliente.
type ExecRequest struct {
	Code     string `json:"code"`
	Language string `json:"language"`
	Input    string `json:"input"`
}

// ErrorMsg descreve um erro retornado pelo compilador ou em tempo de execução.
type ErrorMsg struct {
	Event        string `json:"event"`
	ExceptionMsg string `json:"exception_msg"`
	Line         int    `json:"line"`
	Column       int    `json:"column"`
}

// Ret representa a resposta em caso de erro de compilação.
type Ret struct {
	Code     string   `json:"code"`
	ErrorMsg ErrorMsg `json:"error"`
}

// sanitizeInput validates an input string by checking that it contains only
// allowed characters.
// Returns `true` if the input matches the expected pattern, `false` otherwise.
func sanitizeInput(input string) bool {
	// 1. Delimitadores:
	//    ^			--> início da string
	//    $			--> fim da string
	//
	//    Isso garante que toda a string deve obedecer ao padrão, não apenas uma
	//    parte dela.
	//
	//
	// 2. Grupo principal:
	//    (...)*	--> significa que a sequência interna pode se repetir 0 ou
	// 					mais vezes.
	//
	//    2.1 Conteúdo do grupo
	//		  [\p{Latin}\p{N}]*	--> qualquer número (0 ou mais) de letras latinas
	// 								ou números.
	// 		  \p{N}+[.,]\p{N}+	--> números que podem ter ponto ou vírgula no meio.
	//		  |					--> "ou", então o grupo aceita letras/números
	// 								simples ou números decimais.
	//		  [\s\n]*			--> aceita 0 ou mais espaços ou quebras de linha
	// 								após o grupo anterior.
	pattern := regexp.MustCompile(`^(([\p{Latin}\p{N}]*|\p{N}+[.,]\p{N}+)[\s\n]*)*$`)
	return pattern.MatchString(input)
}

// Helper function to safely convert string to integer
func toInt(str string) int {
	val, err := strconv.Atoi(str)
	if err != nil {
		return 0
	}
	return val
}

var debugValgrind = false

// Handler trata requisições HTTP para execução de código enviado pelo usuário.
func Handler(context web.Context) error {
	// Objeto que representa a requisição do usuário.
	userRequest := ExecRequest{}

	// Tenta decodificar o corpo da requisição HTTP para dentro da struct
	// `userRequest`.
	//
	// Caso o conteúdo da requisição não corresponda ao formato esperado, o
	// método `Bind` retorna um erro. Nesse caso, respondemos imediatamente com
	// um erro HTTP 400 (Bad Request), informando que a requisição do cliente é
	// inválida.
	if err := context.Bind(&userRequest); err != nil {
		context.Error(http.StatusBadRequest, errors.Wrapf(err, "error binding request"))
		return nil
	}

	// Remove espaços em branco extras do início e do fim do campo `Input` para
	// evitar inconsistências durante a validação.
	userRequest.Input = strings.TrimSpace(userRequest.Input)

	// Valida o conteúdo do campo `Input` usando a função `sanitizeInput`.
	//
	// Caso a validação falhe, significa que o cliente enviou uma entrada inválida.
	// Nesse cenário:
	//   1. É registrado um log em nível de debug mostrando o valor rejeitado.
	//   2. Retorna-se imediatamente uma resposta HTTP 400 (Bad Request), no formato
	// 		JSON, informando que a entrada é inválida.
	if !sanitizeInput(userRequest.Input) {
		log.Debug().Msgf("invalid_input: \"%s\"", userRequest.Input)
		return context.JSON(http.StatusBadRequest, map[string]string{"message": "invalid_input"})
	}

	// Registra em nível de debug o código-fonte enviado pelo cliente através do
	// campo `Code`. 
	log.Debug().Msgf("%s", userRequest.Code)

	// Constrói a definição da tarefa que será submetida ao engine, a partir dos
	// dados recebidos no objeto `userRequest`.
	//	
	// Caso a construção da tarefa falhe, retorna-se imediatamente um erro
	// HTTP 400 (Bad Request).
	task, err := buildTask(userRequest)
	if err != nil {
		context.Error(http.StatusBadRequest, err)
		return nil
	}

	result := make(chan string)

	listener := func(j *tork.Job) {
		if j.State == tork.JobStateCompleted {
			result <- j.Execution[0].Result
		} else {
			result <- j.Execution[0].Error
		}
	}

	inputN := &input.Job{
		Name:  "code execution",
		Tasks: []input.Task{task},
	}

	job, err := engine.SubmitJob(context.Request().Context(), inputN, listener)

	if err != nil {
		context.Error(http.StatusBadRequest, errors.Wrapf(err, "error executing code"))
		return nil
	}

	log.Debug().Msgf("job %s submitted", job.ID)

	select {
	case r := <-result:
		if debugValgrind {
			return context.JSON(http.StatusOK, r)
		} else {
			// Define the regex pattern with the filename "usercode.c"
			pattern := `usercode(.c|.cpp):(\d+):(\d+):.+?(error:.*)`

			// Compile the regular expression
			re := regexp.MustCompile(pattern)

			// Check if the regex matches the input string
			isMatch := re.MatchString(r)

			var jsonData map[string]interface{}
			if !isMatch {
				if err := json.Unmarshal([]byte(r), &jsonData); err != nil {
					log.Debug().Msgf("unknown_json_parsing_error: %s", err.Error())
					log.Debug().Msg(r)
					return context.JSON(http.StatusBadRequest, map[string]string{"message": "unknown_error"})
				}
				return context.JSON(http.StatusOK, jsonData)
			} else {
				err := json.Unmarshal([]byte(handleGccError(userRequest.Code, r)), &jsonData)
				if err != nil {
					return err
				}
				return context.JSON(http.StatusBadRequest, jsonData)
			}

		}

	case <-context.Done():
		return context.JSON(http.StatusGatewayTimeout, map[string]string{"message": "timeout"})
	}
}

func buildTask(er ExecRequest) (input.Task, error) {
	var image string
	var run string
	var filename string
	var compiler string
	var language string

	image = "gcc-compiler:latest"

	switch strings.TrimSpace(er.Language) {
	case "":
		return input.Task{}, errors.Errorf("require: language")
	case "c++":
		compiler = "g++"
		filename = "usercode.cpp"
		language = "c++"

	case "c":
		compiler = "gcc"
		filename = "usercode.c"
		language = "c"

	default:
		return input.Task{}, errors.Errorf("unknown language: %s", er.Language)
	}

	run =
		// Move file
		"mv " + filename + " /tmp/user_code/" + filename + "; " +

			// Create file with the user input in the same directory of the program source file
			"echo \"" + er.Input + "\" > /tmp/user_code/programInput.txt; " +

			// Compile user code without warnings (-w). stderr output is passed to TORK_OUTPUT (in case of compiling error)
			compiler + " -w -ggdb -O0 -fno-omit-frame-pointer -o /tmp/user_code/usercode /tmp/user_code/" + filename + " 2> $TORK_OUTPUT; " +

			// If the TORK_OUTPUT is not empty, i.e., an error happened, do nothing
			"[ -s \"${TORK_OUTPUT}\" ] || "

	if debugValgrind {
		run += "cat /tmp/user_code/usercode.vgtrace > $TORK_OUTPUT"
	} else {
		run += "python3 /tmp/parser/wsgi_backend.py " + language + " > $TORK_OUTPUT"
	}

	return input.Task{
		Name:    "execute code",
		Image:   image,
		Run:     run,
		Timeout: "20s",
		Limits: &input.Limits{
			CPUs:   "1",
			Memory: "1000m",
		},
		Files: map[string]string{
			filename: er.Code,
		},
	}, nil
}

func handleGccError(code string, gccStderr string) string {

	exceptionMsg := "unknown compiler error"
	errorType := "uncaught_exception"
	lineNumber := 0
	columnNumber := 0

	println(gccStderr)

	// Split gccStderr into lines and process
	lines := strings.Split(gccStderr, "\n")
	for _, line := range lines {
		// Try to match the error format
		re := regexp.MustCompile(`usercode(.c|.cpp):(?P<Line>\d+):(?P<Column>\d+):.+?(?P<Error>error:.*$)`)
		matches := re.FindStringSubmatch(line)
		if matches != nil {
			// Extract the line and column number and the error message
			lineNumber = toInt(matches[re.SubexpIndex("Line")])
			columnNumber = toInt(matches[re.SubexpIndex("Column")])
			exceptionMsg = strings.TrimSpace(matches[re.SubexpIndex("Error")])
			errorType = "compiler"
			break
		}

		// Handle custom-defined errors from include path
		if strings.Contains(line, "#error") {
			// Extract the error message after '#error'
			exceptionMsg = strings.TrimSpace(strings.Split(line, "#error")[1])
			break
		}

		// Handle linker errors (undefined reference)
		if strings.Contains(line, "undefined ") {
			parts := strings.Split(line, ":")
			exceptionMsg = strings.TrimSpace(parts[len(parts)-1])
			// Match file path and line number
			if strings.Contains(parts[0], "usercode.c") || strings.Contains(parts[0], "usercode.cpp") {
				lineNumber = toInt(parts[1])
			}
			break
		}
	}

	// Prepare the return value
	ret := Ret{
		Code: code,
		ErrorMsg: ErrorMsg{
			Event:        errorType,
			ExceptionMsg: exceptionMsg,
			Line:         lineNumber,
			Column:       columnNumber,
		},
	}

	// Convert to JSON
	retJson, _ := json.Marshal(ret)

	return string(retJson)
}
