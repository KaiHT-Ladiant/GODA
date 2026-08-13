package googleauth

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

var scopes = []string{"https://www.googleapis.com/auth/calendar"}

type Installed struct {
	ClientID     string   `json:"client_id"`
	ClientSecret string   `json:"client_secret"`
	RedirectURIs []string `json:"redirect_uris"`
}

type credentialsFile struct {
	Installed *Installed `json:"installed"`
	Web       *Installed `json:"web"`
}

func LoadOAuthConfig(credentialsPath string) (*oauth2.Config, error) {
	b, err := os.ReadFile(credentialsPath)
	if err != nil {
		return nil, fmt.Errorf("credentials.json 없음: %w", err)
	}
	var cf credentialsFile
	if err := json.Unmarshal(b, &cf); err != nil {
		return nil, err
	}
	inst := cf.Installed
	if inst == nil {
		inst = cf.Web
	}
	if inst == nil {
		return nil, fmt.Errorf("credentials.json에 installed/web 클라이언트 정보가 없습니다")
	}
	return &oauth2.Config{
		ClientID:     inst.ClientID,
		ClientSecret: inst.ClientSecret,
		Endpoint:     google.Endpoint,
		Scopes:       scopes,
		RedirectURL:  "http://127.0.0.1:0",
	}, nil
}

func GetClient(ctx context.Context, credentialsPath, tokenPath string) (*http.Client, error) {
	cfg, err := LoadOAuthConfig(credentialsPath)
	if err != nil {
		return nil, err
	}
	tok, err := tokenFromFile(tokenPath)
	if err == nil {
		ts := cfg.TokenSource(ctx, tok)
		ntok, err := ts.Token()
		if err == nil {
			if ntok.AccessToken != tok.AccessToken || ntok.Expiry != tok.Expiry {
				_ = saveToken(tokenPath, ntok)
			}
			return oauth2.NewClient(ctx, ts), nil
		}
	}
	tok, err = loginInteractive(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := saveToken(tokenPath, tok); err != nil {
		return nil, err
	}
	return cfg.Client(ctx, tok), nil
}

func HasValidToken(tokenPath string) bool {
	tok, err := tokenFromFile(tokenPath)
	if err != nil {
		return false
	}
	return tok.Valid() || tok.RefreshToken != ""
}

func Login(ctx context.Context, credentialsPath, tokenPath string) error {
	cfg, err := LoadOAuthConfig(credentialsPath)
	if err != nil {
		return err
	}
	tok, err := loginInteractive(ctx, cfg)
	if err != nil {
		return err
	}
	return saveToken(tokenPath, tok)
}

func tokenFromFile(path string) (*oauth2.Token, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	tok := &oauth2.Token{}
	err = json.NewDecoder(f).Decode(tok)
	return tok, err
}

func saveToken(path string, tok *oauth2.Token) error {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(tok)
}

func loginInteractive(ctx context.Context, cfg *oauth2.Config) (*oauth2.Token, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	cfg.RedirectURL = fmt.Sprintf("http://127.0.0.1:%d/", port)

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if errMsg := r.URL.Query().Get("error"); errMsg != "" {
			errCh <- fmt.Errorf("oauth error: %s", errMsg)
			fmt.Fprint(w, "로그인 실패. 창을 닫아도 됩니다.")
			return
		}
		code := r.URL.Query().Get("code")
		if code == "" {
			http.Error(w, "missing code", http.StatusBadRequest)
			return
		}
		fmt.Fprint(w, "Google 로그인 완료. 이 창을 닫고 앱으로 돌아가세요.")
		codeCh <- code
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	authURL := cfg.AuthCodeURL("state-token", oauth2.AccessTypeOffline, oauth2.ApprovalForce)
	_ = openBrowser(authURL)

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case err := <-errCh:
		return nil, err
	case code := <-codeCh:
		return cfg.Exchange(ctx, code)
	case <-time.After(3 * time.Minute):
		return nil, fmt.Errorf("Google 로그인 시간 초과")
	}
}

func openBrowser(url string) error {
	switch runtime.GOOS {
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	case "darwin":
		return exec.Command("open", url).Start()
	default:
		return exec.Command("xdg-open", url).Start()
	}
}
