package notification

import (
	"testing"
	"time"

	"github.com/soulteary/gorge/go/internal/notification/hub"
)

func TestDbgProdDelivery(t *testing.T) {
	baseURL, messages := newClientServer(t)
	conn := dialTo(t, baseURL, "/~prod/")
	sendCommand(t, conn, "subscribe", []string{"PHID-USER-x"})
	syncCommands(t, conn)
	st := messages.Status("prod")
	t.Logf("prod clients active=%d total=%d", st.ClientsActive, st.ClientsTotal)
	dst := messages.Status(defaultInstance)
	t.Logf("default clients active=%d total=%d", dst.ClientsActive, dst.ClientsTotal)
	messages.Publish("prod", hub.Message{"type": "notification", "key": "right-instance", "subscribers": []string{"PHID-USER-x"}})
	_ = time.Second
	got := readMessage(t, conn)
	t.Logf("got %v", got)
}
