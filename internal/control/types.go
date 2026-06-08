package control

import (
	"net/http"
	"strings"
)

type Client struct {
	HubURL string
	Token  string
	HTTP   *http.Client
}

func New(hubURL, token string) Client {
	return Client{HubURL: strings.TrimRight(hubURL, "/"), Token: token, HTTP: http.DefaultClient}
}
