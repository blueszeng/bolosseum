package main

import (
	"fmt"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/gin-gonic/gin"
	"github.com/googollee/go-socket.io"
	"github.com/moul/bolosseum"
	"github.com/moul/bolosseum/bots"
	"github.com/moul/bolosseum/bots/filebot"
	"github.com/moul/bolosseum/bots/httpbot"
	"github.com/moul/bolosseum/bots/stupidbot"
	"github.com/moul/bolosseum/games"
	"github.com/moul/bolosseum/games/coinflip"
	"github.com/moul/bolosseum/games/connectfour"
	"github.com/moul/bolosseum/games/russianbullet"
	"github.com/moul/bolosseum/games/tictactoe"
	"github.com/moul/bolosseum/stupid-ias"
	"github.com/moul/bolosseum/stupid-ias/coinflip"
	"github.com/moul/bolosseum/stupid-ias/connectfour"
	"github.com/moul/bolosseum/stupid-ias/tictactoe"
	"github.com/urfave/cli"
)

type APIStep struct {
	Type string      `json:"type",omitempty`
	Data interface{} `json:"data",omitempty`
}

type APIResult struct {
	Steps []APIStep `json:"steps",omitempty`
}

var availableGames = []string{
	"coinflip",
	"connectfour",
	"russianbullet",
	"tictactoe",
}

func getGame(gameName string) (games.Game, error) {
	switch gameName {
	case "coinflip":
		return coinflip.NewGame()
	case "connectfour":
		return connectfour.NewGame()
	case "russianbullet":
		return russianbullet.NewGame()
	case "tictactoe":
		return tictactoe.NewGame()
	default:
		return nil, fmt.Errorf("unknown game %q", gameName)
	}
}

func getStupidIA(iaPath string) (stupidias.StupidIA, error) {
	logrus.Warnf("Getting stupid IA %q", iaPath)
	switch iaPath {
	case "connectfour":
		return stupidconnectfour.NewIA()
	case "coinflip":
		return stupidcoinflip.NewIA()
	case "tictactoe":
		return stupidtictactoe.NewIA()
	default:
		return nil, fmt.Errorf("unknown stupid IA %q", iaPath)
	}
}

func getBot(botPath string, game games.Game) (bots.Bot, error) {
	logrus.Warnf("Getting bot %q", botPath)

	if botPath == "stupid" {
		botPath = fmt.Sprintf("stupid://%s", game.Name())
	}

	splt := strings.Split(botPath, "://")
	if len(splt) != 2 {
		return nil, fmt.Errorf("invalid bot path")
	}

	scheme := splt[0]
	path := splt[1]

	switch scheme {
	case "file":
		return filebot.NewBot(path)
	case "http+get":
		return httpbot.NewBot(path, "GET", "http")
	case "http+post", "http":
		return httpbot.NewBot(path, "POST", "http")
	case "https+get":
		return httpbot.NewBot(path, "GET", "https")
	case "https+post", "https":
		return httpbot.NewBot(path, "POST", "https")
	case "stupid":
		ia, err := getStupidIA(path)
		if err != nil {
			return nil, err
		}
		return stupidbot.NewStupidBot(path, ia)
	default:
		return nil, fmt.Errorf("invalid bot scheme: %q (%q)", scheme, path)
	}
}

func main() {
	// seed random
	rand.Seed(time.Now().UTC().UnixNano())

	// configure CLI
	app := cli.NewApp()
	app.Name = "bolosseum"
	app.Usage = "colosseum for bots"
	app.Version = bolosseum.VERSION

	app.Commands = []cli.Command{
		{
			Name:   "run",
			Usage:  "Start a battle",
			Action: run,
		},
		{
			Name:   "list-games",
			Usage:  "List games",
			Action: listGames,
		},
		{
			Name:   "server",
			Usage:  "Start a bolosseum web server",
			Action: server,
		},
	}
	if err := app.Run(os.Args); err != nil {
		logrus.Fatalf("%v", err)
	}
}

var indexHTML = `<html>
  <head>
    <title>Bolosseum</title>
  </head>
  <body>
    <h1>Bolosseum</h1>
  </body>
</html>`

func translateSteps(inputSteps chan games.GameStep, outputSteps chan APIStep, finished chan bool) {
	for step := range inputSteps {
		if step.QuestionMessage != nil {
			outputSteps <- APIStep{Type: "question", Data: *step.QuestionMessage}
		} else if step.ReplyMessage != nil {
			outputSteps <- APIStep{Type: "reply", Data: step.ReplyMessage}
		} else if step.Error != nil {
			outputSteps <- APIStep{Type: "error", Data: step.Error}
			close(inputSteps)
		} else if step.Message != "" {
			outputSteps <- APIStep{Type: "message", Data: step.Message}
		} else if step.Winner != nil {
			outputSteps <- APIStep{Type: "winner", Data: step.Winner.Name()}
			close(inputSteps)
		} else if step.Draw {
			outputSteps <- APIStep{Type: "draw"}
			close(inputSteps)
		} else {
			outputSteps <- APIStep{Type: "error", Data: fmt.Errorf("Unknown message type: %v", step)}
			close(inputSteps)
		}
	}
	finished <- true
}

func server(c *cli.Context) error {
	r := gin.Default()
	r.LoadHTMLGlob("web/*")
	r.GET("/", func(c *gin.Context) {
		//c.Header("Content-Type", "text/html")
		//c.String(http.StatusOK, indexHTML)
		c.HTML(http.StatusOK, "index.tmpl", nil)
	})
	r.POST("/run", func(c *gin.Context) {
		gameName := c.PostForm("game")
		bot1URL, err := url.QueryUnescape(c.PostForm("bot1"))
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{
				"error":  "Invalid bot1 parameter",
				"detail": err,
			})
			return
		}
		bot2URL, err := url.QueryUnescape(c.PostForm("bot2"))
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{
				"error":  "Invalid bot2 parameter",
				"detail": err,
			})
			return
		}

		if gameName == "" || bot1URL == "" || bot2URL == "" {
			c.JSON(http.StatusNotFound, gin.H{
				"error": "Missing parameters",
			})
			return
		}

		// initialize game
		logrus.Warnf("Initializing game %q", gameName)
		game, err := getGame(gameName)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{
				"error": "No such game",
			})
			return
		}
		logrus.Warnf("Game: %q: %q", game.Name(), game)

		args := []string{bot1URL, bot2URL}

		if err = game.CheckArgs(args); err != nil {
			c.JSON(http.StatusNotFound, gin.H{
				"error":  "Invalid parameters",
				"detail": err,
			})
		}

		// initialize bots
		hasError := false
		for _, botPath := range args {
			bot, err := getBot(botPath, game)
			if err != nil {
				hasError = true
				logrus.Errorf("Failed to initialize bot %q", bot)
			} else {
				logrus.Warnf("Registering bot %q", bot.Path())
				game.RegisterBot(bot)
			}
		}
		if hasError {
			c.JSON(http.StatusNotFound, gin.H{
				"error":  "bot registering error",
				"detail": err,
			})
			return
		}

		// run
		inputSteps := make(chan games.GameStep)
		outputSteps := make(chan APIStep)
		finished := make(chan bool)
		go translateSteps(inputSteps, outputSteps, finished)

		var result APIResult
		go func() {
			for step := range outputSteps {
				result.Steps = append(result.Steps, step)
			}
		}()

		if err = game.Run("gameid", inputSteps); err != nil {
			logrus.Errorf("Run error: %v", err)
		}

		select {
		case <-finished:
		}

		// print ascii output
		result.Steps = append(result.Steps, APIStep{Type: "ascii-output", Data: string(game.GetAsciiOutput())})

		c.JSON(http.StatusOK, result)
	})

	sioServer, err := socketio.NewServer(nil)
	if err != nil {
		return err
	}

	sioServer.On("connection", func(so socketio.Socket) {
		logrus.Warnf("sio connection: %v", so)
		so.On("run", func(data struct {
			Game string `json:"game"`
			Bot1 string `json:"bot1"`
			Bot2 string `json:"bot2"`
		}) error {
			logrus.Warnf("sio run: %v", data)

			gameName := data.Game
			bot1URL := data.Bot1
			bot2URL := data.Bot2

			if gameName == "" || bot1URL == "" || bot2URL == "" {
				return fmt.Errorf("Missing parameters")
			}

			// initialize game
			logrus.Warnf("Initializing game %q", gameName)
			game, err := getGame(gameName)
			if err != nil {
				return fmt.Errorf("No such game")
			}

			logrus.Warnf("Game: %q: %q", game.Name(), game)

			args := []string{bot1URL, bot2URL}

			if err = game.CheckArgs(args); err != nil {
				return fmt.Errorf("Invalid parameters: %v", err)
			}

			// initialize bots
			for _, botPath := range args {
				bot, err := getBot(botPath, game)
				if err != nil {
					return fmt.Errorf("Failed to initialize bot %q", bot)
				}
				logrus.Warnf("Registering bot %q", bot.Path())
				game.RegisterBot(bot)
			}

			// run
			inputSteps := make(chan games.GameStep)
			outputSteps := make(chan APIStep)
			finished := make(chan bool)
			go translateSteps(inputSteps, outputSteps, finished)

			go func() {
				for step := range outputSteps {
					so.Emit("step", step)
				}
			}()

			if err = game.Run("gameid", inputSteps); err != nil {
				logrus.Errorf("Run error: %v", err)
			}

			select {
			case <-finished:
			}

			// print ascii output
			if output := game.GetAsciiOutput(); len(output) > 0 {
				so.Emit("step", APIStep{Type: "ascii-output", Data: string(output)})
			}

			so.Emit("disconnect")
			return nil
		})

		so.On("disconnection", func() {
			logrus.Warnf("on disconnect")
		})
	})

	sioServer.On("error", func(so socketio.Socket, err error) {
		logrus.Errorf("sio error: %v -> %v", so, err)
	})

	http.Handle("/socket.io/", sioServer)
	http.Handle("/", r)
	//return r.Run(":9000")
	addr := ":9000"
	logrus.Warnf("Listening and serving HTTP on %s", addr)
	return http.ListenAndServe(addr, nil)
}

func listGames(c *cli.Context) error {
	fmt.Println("Games:")
	for _, game := range availableGames {
		fmt.Printf("- %s\n", game)
	}
	return nil
}

func run(c *cli.Context) error {
	args := c.Args()
	if len(args) < 1 {
		return cli.NewExitError("You need to specify the game", -1)
	}

	// initialize game
	logrus.Warnf("Initializing game %q", args[0])
	game, err := getGame(args[0])
	if err != nil {
		return cli.NewExitError(fmt.Sprintf("No such game %q", args[0]), -1)
	}
	logrus.Warnf("Game: %q: %q", game.Name(), game)

	if err = game.CheckArgs(args[1:]); err != nil {
		return cli.NewExitError(fmt.Sprintf("%v", err), -1)
	}

	// initialize bots
	hasError := false
	for _, botPath := range args[1:] {
		bot, err := getBot(botPath, game)
		if err != nil {
			hasError = true
			logrus.Errorf("Failed to initialize bot %q", bot)
		} else {
			logrus.Warnf("Registering bot %q", bot.Path())
			game.RegisterBot(bot)
		}
	}
	if hasError {
		return cli.NewExitError("Invalid bots", -1)
	}

	// run
	steps := make(chan games.GameStep)
	finished := make(chan bool)
	go func() {
		for step := range steps {
			if step.QuestionMessage != nil {
				logrus.Warnf("bot-%d << %v", step.QuestionMessage.PlayerIndex, *step.QuestionMessage)
			} else if step.ReplyMessage != nil {
				logrus.Warnf("bot-%d >> %v", step.ReplyMessage.PlayerIndex, *step.ReplyMessage)
			} else if step.Error != nil {
				logrus.Errorf("%v", step.Error)
				close(steps)
			} else if step.Message != "" {
				logrus.Warnf("message: %s", step.Message)
			} else if step.Winner != nil {
				logrus.Warnf("winner: %s", step.Winner.Name())
				close(steps)
			} else if step.Draw {
				logrus.Warnf("Draw")
				close(steps)
			} else {
				logrus.Errorf("Unknown message type: %v", step)
				close(steps)
			}
		}
		finished <- true
	}()

	if err = game.Run("gameid", steps); err != nil {
		logrus.Errorf("Run error: %v", err)
	}

	select {
	case <-finished:
	}

	// print ascii output
	if output := game.GetAsciiOutput(); len(output) > 0 {
		fmt.Printf("%s", output)
	}

	return nil
}
