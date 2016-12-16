package main // In Go, executable commands must always use package main

/*
	Imports
*/

import (
	"github.com/Zamiell/isaac-racing-server/models"

	"net/http" // For establishing an HTTP server
	"os"       // For logging and reading environment variables
	"strconv"  // For converting the port number
	"sync"     // For locking and unlocking the connection map
	"time"     // For dealing with timestamps

	"github.com/bmizerany/pat"       // For HTTP routing
	"github.com/didip/tollbooth"     // For rate-limiting login requests
	"github.com/gorilla/context"     // For cookie sessions (1/2)
	"github.com/gorilla/sessions"    // For cookie sessions (2/2)
	"github.com/joho/godotenv"       // For reading environment variables that contain secrets
	"github.com/op/go-logging"       // For logging
	"github.com/tdewolff/minify"     // For minification (1/3)
	"github.com/tdewolff/minify/css" // For minification (2/3)
	"github.com/tdewolff/minify/js"  // For minification (3/3)
	"github.com/trevex/golem"        // The Golem WebSocket framework
)

/*
	Constants
*/

const (
	sessionName   = "isaac.sid"
	domain        = "isaacracing.net"
	auth0Domain   = "isaacserver.auth0.com"
	useSSL        = true
	sslCertFile   = "/etc/letsencrypt/live/" + domain + "/fullchain.pem"
	sslKeyFile    = "/etc/letsencrypt/live/" + domain + "/privkey.pem"
	rateLimitRate = 120 // In commands sent
	rateLimitPer  = 60  // In seconds
)

/*
	Global variables
*/

var (
	projectPath   = os.Getenv("GOPATH") + "/src/github.com/Zamiell/isaac-racing-server"
	log           = logging.MustGetLogger("isaac")
	db            *models.Models
	sessionStore  *sessions.CookieStore
	commandMutex  = &sync.Mutex{} // Used to prevent race conditions
	roomManager   = golem.NewRoomManager()
	pmManager     = golem.NewRoomManager()
	connectionMap = struct {
		// Maps are not safe for concurrent use: https://blog.golang.org/go-maps-in-action
		sync.RWMutex
		m map[string]*ExtendedConnection
	}{m: make(map[string]*ExtendedConnection)}
	chatRoomMap = struct {
		// Maps are not safe for concurrent use: https://blog.golang.org/go-maps-in-action
		sync.RWMutex
		m map[string][]User
	}{m: make(map[string][]User)}
	achievementMap map[int][]string
)

/*
	No directory listing stuff from: https://marc.ttias.be/golang-nuts/2016-03/msg00888.php
*/

type justFilesFilesystem struct {
	fs http.FileSystem
}

func (fs justFilesFilesystem) Open(name string) (http.File, error) {
	f, err := fs.fs.Open(name)
	if err != nil {
		return nil, err
	}
	return neuteredReaddirFile{f}, nil
}

type neuteredReaddirFile struct {
	http.File
}

func (f neuteredReaddirFile) Readdir(count int) ([]os.FileInfo, error) {
	return nil, nil
}

/*
	HTTP to HTTPS redirect
*/

func HTTPRedirect(w http.ResponseWriter, req *http.Request) {
	http.Redirect(w, req, "https://"+req.Host+req.URL.String(), http.StatusMovedPermanently)
}

/*
	Program entry point
*/

func main() {
	// Configure logging: http://godoc.org/github.com/op/go-logging#Formatter
	loggingBackend := logging.NewLogBackend(os.Stdout, "", 0)
	logFormat := logging.MustStringFormatter( // https://golang.org/pkg/time/#Time.Format
		`%{time:Mon Jan 2 15:04:05 MST 2006} - %{level:.4s} - %{shortfile} - %{message}`,
	)
	loggingBackendFormatted := logging.NewBackendFormatter(loggingBackend, logFormat)
	logging.SetBackend(loggingBackendFormatted)

	// Welcome message
	log.Info("-----------------------------")
	log.Info("Starting isaac-racing-server.")
	log.Info("-----------------------------")

	// Load the .env file which contains environment variables with secret values
	err := godotenv.Load(projectPath + "/.env")
	if err != nil {
		log.Fatal("Failed to load .env file:", err)
	}

	// Create a session store
	sessionSecret := os.Getenv("SESSION_SECRET")
	sessionStore = sessions.NewCookieStore([]byte(sessionSecret))
	maxAge := 5 // 5 seconds
	if useSSL == true {
		sessionStore.Options = &sessions.Options{
			Domain:   domain,
			Path:     "/",
			MaxAge:   maxAge,
			Secure:   true, // Only send the cookie over HTTPS: https://www.owasp.org/index.php/Testing_for_cookies_attributes_(OTG-SESS-002)
			HttpOnly: true, // Mitigate XSS attacks: https://www.owasp.org/index.php/HttpOnly
		}
	} else {
		sessionStore.Options = &sessions.Options{
			Domain:   domain,
			Path:     "/",
			MaxAge:   maxAge,
			HttpOnly: true, // Mitigate XSS attacks: https://www.owasp.org/index.php/HttpOnly
		}
	}

	// Initialize the database model
	if db, err = models.GetModels(projectPath + "/database.sqlite"); err != nil {
		log.Fatal("Failed to open the database:", err)
	}

	// Clean up any non-started races before we start
	if nonStartedRaces, err := db.Races.Cleanup(); err != nil {
		log.Fatal("Failed to cleanup the leftover races:", err)
	} else {
		for _, raceID := range nonStartedRaces {
			log.Info("Deleted race", raceID, "during starting cleanup.")
		}
	}

	// Initiate the "end of race" function for each of the non-finished races in 30 minutes
	// (this is normally initiated on race start)
	if startedRaces, err := db.Races.GetCurrentRaces(); err != nil {
		log.Fatal("Failed to start  the leftover races:", err)
	} else {
		for _, race := range startedRaces {
			go raceCheckStart3(race.ID)
		}
	}

	// Initialize the achievements
	achievementsInit()

	// Start the Twitch bot
	go twitchInit()

	// Create a WebSocket router using the Golem framework
	router := golem.NewRouter()
	router.SetConnectionExtension(NewExtendedConnection)
	router.OnHandshake(validateSession)
	router.OnConnect(connOpen)
	router.OnClose(connClose)

	/*
		The websocket commands
	*/

	// Chat commands
	router.On("roomJoin", roomJoin)
	router.On("roomLeave", roomLeave)
	router.On("roomMessage", roomMessage)
	router.On("privateMessage", privateMessage)
	router.On("roomListAll", roomListAll)

	// Race commands
	router.On("raceCreate", raceCreate)
	router.On("raceJoin", raceJoin)
	router.On("raceLeave", raceLeave)
	router.On("raceReady", raceReady)
	router.On("raceUnready", raceUnready)
	router.On("raceRuleset", raceRuleset)
	router.On("raceFinish", raceFinish)
	router.On("raceQuit", raceQuit)
	router.On("raceComment", raceComment)
	router.On("raceItem", raceItem)
	router.On("raceFloor", raceFloor)

	// Profile commands
	router.On("profileGet", profileGet)
	router.On("profileSetUsername", profileSetUsername)
	router.On("profileSetStream", profileSetStream)
	router.On("profileSetTwitchBotEnabled", profileSetTwitchBotEnabled)
	router.On("profileSetTwitchBotDelay", profileSetTwitchBotDelay)

	// Admin commands
	router.On("adminBan", adminBan)
	router.On("adminUnban", adminUnban)
	router.On("adminBanIP", adminBanIP)
	router.On("adminUnbanIP", adminUnbanIP)
	router.On("adminSquelch", adminSquelch)
	router.On("adminUnsquelch", adminUnsquelch)
	router.On("adminPromote", adminPromote)
	router.On("adminDemote", adminDemote)

	// Miscellaneous
	router.On("logout", logout)
	router.On("debug", debug)

	/*
		HTTP stuff
	*/

	// Minify CSS and JS
	m := minify.New()
	m.AddFunc("text/css", css.Minify)
	for _, fileName := range []string{"main", "ie8"} {
		inputFile, _ := os.Open("public/css/" + fileName + ".css")
		outputFile, _ := os.Create("public/css/" + fileName + ".min.css")
		if err := m.Minify("text/css", outputFile, inputFile); err != nil {
			log.Error("Failed to minify \""+fileName+".css\":", err)
		}
	}
	m.AddFunc("text/javascript", js.Minify)
	for _, fileName := range []string{"main", "util"} {
		inputFile, _ := os.Open("public/js/" + fileName + ".js")
		outputFile, _ := os.Create("public/js/" + fileName + ".min.js")
		if err := m.Minify("text/javascript", outputFile, inputFile); err != nil {
			log.Error("Failed to minify \""+fileName+".js\":", err)
		}
	}

	// Set up the Pat HTTP router
	p := pat.New()
	p.Get("/", tollbooth.LimitFuncHandler(tollbooth.NewLimiter(1, time.Second), httpHome))
	p.Get("/news", tollbooth.LimitFuncHandler(tollbooth.NewLimiter(1, time.Second), httpNews))
	p.Get("/races", tollbooth.LimitFuncHandler(tollbooth.NewLimiter(1, time.Second), httpRaces))
	p.Get("/profiles", tollbooth.LimitFuncHandler(tollbooth.NewLimiter(1, time.Second), httpProfiles))
	p.Get("/leaderboards", tollbooth.LimitFuncHandler(tollbooth.NewLimiter(1, time.Second), httpLeaderboards))
	p.Get("/info", tollbooth.LimitFuncHandler(tollbooth.NewLimiter(1, time.Second), httpInfo))
	p.Get("/download", tollbooth.LimitFuncHandler(tollbooth.NewLimiter(1, time.Second), httpDownload))
	p.Post("/login", tollbooth.LimitFuncHandler(tollbooth.NewLimiter(1, time.Second), loginHandler))

	// Assign functions to URIs
	http.Handle("/", p)
	http.Handle("/public/", http.StripPrefix("/public/", http.FileServer(justFilesFilesystem{http.Dir("public")})))
	http.HandleFunc("/ws", router.Handler())

	/*
		Start the server
	*/

	// Figure out the port that we are using for the HTTP server
	var port int
	if useSSL == true {
		go http.ListenAndServe(":80", http.HandlerFunc(HTTPRedirect))
		port = 443
	} else {
		port = 80
	}

	// Listen and serve
	log.Info("Listening on port " + strconv.Itoa(port) + ".")
	if useSSL == true {
		if err := http.ListenAndServeTLS(
			":"+strconv.Itoa(port), // Nothing before the colon implies 0.0.0.0
			sslCertFile,
			sslKeyFile,
			context.ClearHandler(http.DefaultServeMux), // We wrap with context.ClearHandler or else we will leak memory: http://www.gorillatoolkit.org/pkg/sessions
		); err != nil {
			log.Fatal("ListenAndServeTLS failed:", err)
		}
	} else {
		// Listen and serve (HTTP)
		if err := http.ListenAndServe(
			":"+strconv.Itoa(port),                     // Nothing before the colon implies 0.0.0.0
			context.ClearHandler(http.DefaultServeMux), // We wrap with context.ClearHandler or else we will leak memory: http://www.gorillatoolkit.org/pkg/sessions
		); err != nil {
			log.Fatal("ListenAndServeTLS failed:", err)
		}
	}
}
