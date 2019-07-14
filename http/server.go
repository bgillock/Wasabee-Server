package wasabeehttps

import (
	"context"
	"crypto/tls"
	"html/template"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"github.com/gorilla/sessions"
	"github.com/wasabee-project/Wasabee-Server"
	// XXX gorilla has logging middleware, use that instead?
	"github.com/unrolled/logger"
)

// Configuration is the main configuration data for the https server
// an initial config is sent from main() and that is updated with defaults
// in the initializeConfig function
type Configuration struct {
	ListenHTTPS       string
	FrontendPath      string
	Root              string
	path              string
	domain            string
	oauthStateString  string
	CertDir           string
	GoogleClientID    string
	GoogleSecret      string
	googleOauthConfig *oauth2.Config
	store             *sessions.CookieStore
	sessionName       string
	CookieSessionKey  string
	TemplateSet       map[string]*template.Template // allow multiple translations
	Logfile           string
	srv               *http.Server
	logfileHandle     *os.File
	unrolled          *logger.Logger
	scanners          map[string]int64
}

var config Configuration

const jsonType = "application/json; charset=UTF-8"
const jsonTypeShort = "application/json"
const me = "/me"
const login = "/login"
const callback = "/callback"
const aptoken = "/aptok"
const apipath = "/api/v1"
const appUserAgent = "(dart:io)"

// initializeConfig will normalize the options and create the "config" object.
func initializeConfig(initialConfig Configuration) {
	config = initialConfig

	config.Root = strings.TrimSuffix(config.Root, "/")

	// Extract "path" fron "root"
	rootParts := strings.SplitAfterN(config.Root, "/", 4) // https://example.org/[grab this part]
	config.path = ""
	if len(rootParts) > 3 { // Otherwise: application in root folder
		config.path = rootParts[3]
	}
	config.path = strings.TrimSuffix("/"+strings.TrimPrefix(config.path, "/"), "/")

	rootParts = strings.SplitN(strings.ToLower(config.Root), "://", 2)
	config.domain = strings.Split(rootParts[len(rootParts)-1], "/")[0]

	// used for templates
	wasabee.SetWebroot(config.Root)
	wasabee.SetWebAPIPath(apipath)

	if config.GoogleClientID == "" {
		wasabee.Log.Error("GOOGLE_CLIENT_ID unset: logins will fail")
	}
	if config.GoogleSecret == "" {
		wasabee.Log.Error("GOOGLE_SECRET unset: logins will fail")
	}

	config.googleOauthConfig = &oauth2.Config{
		RedirectURL:  config.Root + callback,
		ClientID:     config.GoogleClientID,
		ClientSecret: config.GoogleSecret,
		Scopes:       []string{"https://www.googleapis.com/auth/userinfo.email"},
		Endpoint:     google.Endpoint,
	}
	wasabee.Log.Debugf("ClientID: " + config.googleOauthConfig.ClientID)
	wasabee.Log.Debugf("ClientSecret: " + config.googleOauthConfig.ClientSecret)
	config.oauthStateString = wasabee.GenerateName()
	wasabee.Log.Debugf("oauthStateString: " + config.oauthStateString)

	if config.CookieSessionKey == "" {
		wasabee.Log.Error("SESSION_KEY unset: logins will fail")
	} else {
		key := config.CookieSessionKey
		wasabee.Log.Debugf("Session Key: %s", key)
		config.store = sessions.NewCookieStore([]byte(key))
		config.sessionName = "wasabee"
	}

	// certificate directory cleanup
	if config.CertDir == "" {
		wasabee.Log.Error("CERTDIR unset: defaulting to 'certs'")
		config.CertDir = "certs"
	}
	certdir, err := filepath.Abs(config.CertDir)
	config.CertDir = certdir
	if err != nil {
		wasabee.Log.Critical("certificate path could not be resolved.")
		panic(err)
	}
	wasabee.Log.Debugf("Certificate Directory: " + config.CertDir)

	if config.Logfile == "" {
		config.Logfile = "wasabee-https.log"
	}
	wasabee.Log.Infof("https logfile: %s", config.Logfile)
	// #nosec
	config.logfileHandle, err = os.OpenFile(config.Logfile, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0644)
	if err != nil {
		wasabee.Log.Fatal(err)
	}
	config.unrolled = logger.New(logger.Options{
		Prefix: "wasabee",
		Out:    config.logfileHandle,
		IgnoredRequestURIs: []string{
			"/favicon.ico",
			"/apple-touch-icon-precomposed.png",
			"/apple-touch-icon-120x120-precomposed.png",
			"/apple-touch-icon-120x120.png",
			"/apple-touch-icon.png"},
	})
	config.scanners = make(map[string]int64)
}

// templateExecute outputs directly to the ResponseWriter
func templateExecute(res http.ResponseWriter, req *http.Request, name string, data interface{}) error {
	var lang string
	tmp := req.Header.Get("Accept-Language")
	if tmp == "" {
		lang = "en"
	} else {
		lang = strings.ToLower(tmp)[:2]
	}
	_, ok := config.TemplateSet[lang]
	if !ok {
		lang = "en" // default to english if the map doesn't exist
	}

	if err := config.TemplateSet[lang].ExecuteTemplate(res, name, data); err != nil {
		wasabee.Log.Notice(err)
		return err
	}
	return nil
}

// StartHTTP launches the HTTP server which is responsible for the frontend and the HTTP API.
func StartHTTP(initialConfig Configuration) {
	// take the incoming config, add defaults
	initializeConfig(initialConfig)

	// setup the main router an built-in subrouters
	router := setupRouter()

	// serve
	config.srv = &http.Server{
		Handler:           router,
		Addr:              config.ListenHTTPS,
		WriteTimeout:      15 * time.Second,
		ReadTimeout:       15 * time.Second,
		ReadHeaderTimeout: 2 * time.Second,
		TLSConfig: &tls.Config{
			MinVersion:               tls.VersionTLS12,
			CurvePreferences:         []tls.CurveID{tls.CurveP521, tls.CurveP384, tls.CurveP256},
			PreferServerCipherSuites: true,
			CipherSuites: []uint16{
				tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
				tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
				tls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,
				tls.TLS_RSA_WITH_AES_128_GCM_SHA256,
				tls.TLS_RSA_WITH_AES_256_GCM_SHA384,
				tls.TLS_RSA_WITH_AES_256_CBC_SHA,
			},
		},
	}
	wasabee.Log.Noticef("HTTPS server starting on %s, you should be able to reach it at %s", config.ListenHTTPS, config.Root)
	if err := config.srv.ListenAndServeTLS(config.CertDir+"/wasabee.fullchain.pem", config.CertDir+"/wasabee.key"); err != nil {
		wasabee.Log.Errorf("HTTPS server error: %s", err)
		panic(err)
	}
}

// Shutdown forces a graceful shutdown of the https server
func Shutdown() error {
	wasabee.Log.Info("Shutting down HTTPS server")
	if err := config.srv.Shutdown(context.Background()); err != nil {
		wasabee.Log.Error(err)
		return err
	}
	return nil
}

func headersMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(res http.ResponseWriter, req *http.Request) {
		res.Header().Add("Server", "Wasabee-Server")
		res.Header().Add("X-Frame-Options", "deny")
		res.Header().Add("Access-Control-Allow-Origin", "https://intel.ingress.com")
		res.Header().Add("Access-Control-Allow-Methods", "POST, GET, PUT, OPTIONS, HEAD, DELETE")
		res.Header().Add("Access-Control-Allow-Credentials", "true")
		res.Header().Add("Access-Control-Allow-Headers", "Content-Type, Accept")
		next.ServeHTTP(res, req)
	})
}

func scannerMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(res http.ResponseWriter, req *http.Request) {
		i, ok := config.scanners[req.RemoteAddr]
		if ok && i > 30 {
			http.Error(res, "Scanner detected", http.StatusForbidden)
			return
		}
		next.ServeHTTP(res, req)
	})
}

func authMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(res http.ResponseWriter, req *http.Request) {
		ses, err := config.store.Get(req, config.sessionName)
		if err != nil {
			wasabee.Log.Debug(err)
			delete(ses.Values, "nonce")
			delete(ses.Values, "id")
			delete(ses.Values, "loginReq")
			_ = ses.Save(req, res)
			http.Error(res, err.Error(), http.StatusInternalServerError)
			return
		}

		var redirectURL = login
		if req.URL.String()[:len(me)] != me {
			redirectURL = login + "?returnto=" + req.URL.String()
		}

		id, ok := ses.Values["id"]
		if !ok || id == nil {
			// XXX cookie and returnto may be redundant, but cookie wasn't working in early tests
			ses.Values["loginReq"] = req.URL.String()
			_ = ses.Save(req, res)
			http.Redirect(res, req, redirectURL, http.StatusFound)
			return
		}

		gid := wasabee.GoogleID(id.(string))
		if gid.CheckLogout() {
			wasabee.Log.Notice("requested logout")
			http.Redirect(res, req, "/", http.StatusFound)
			return
		}

		in, ok := ses.Values["nonce"]
		if !ok || in == nil {
			wasabee.Log.Error("gid set, but nonce not")
			http.Redirect(res, req, redirectURL, http.StatusFound)
			return
		}
		inNonce := in.(string)
		nonce, pNonce := calculateNonce(gid)

		if inNonce != nonce {
			if inNonce != pNonce {
				// wasabee.Log.Debug("Session timed out for", gid.String())
				ses.Values["nonce"] = "unset"
				_ = ses.Save(req, res)
			} else {
				// wasabee.Log.Debug("Updating to new nonce")
				ses.Values["nonce"] = nonce
				_ = ses.Save(req, res)
			}
		}

		// TBD: if request is from app or IITC, just return http.StatusXXX
		// @Phtiv bult the app to handle the HTML screen, no worries
		if ses.Values["nonce"] == "unset" {
			http.Redirect(res, req, redirectURL, http.StatusFound)
			return
		}
		next.ServeHTTP(res, req)
	})
}

func googleRoute(res http.ResponseWriter, req *http.Request) {
	ret := req.FormValue("returnto")

	ses, err := config.store.Get(req, config.sessionName)
	if err != nil {
		wasabee.Log.Debug(err)
		http.Error(res, err.Error(), http.StatusInternalServerError)
		return
	}
	if ret != "" {
		ses.Values["loginReq"] = ret
	} else {
		ses.Values["loginReq"] = me
	}
	_ = ses.Save(req, res)

	url := config.googleOauthConfig.AuthCodeURL(config.oauthStateString)
	http.Redirect(res, req, url, http.StatusFound)
}

/*
func debugMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(res http.ResponseWriter, req *http.Request) {
		dump, _ := httputil.DumpRequest(req, false)
		wasabee.Log.Debug(string(dump))
		next.ServeHTTP(res, req)
	})
} */
