package handlers

import "github.com/soulteary/gorge/go/internal/platform/conduitclient"

type ConduitClient = conduitclient.Client
type ConduitResponse = conduitclient.Response

func NewConduitClient(baseURL, token string) *ConduitClient {
	return conduitclient.New(baseURL, token)
}
