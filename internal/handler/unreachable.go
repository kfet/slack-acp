package handler

import "github.com/kfet/acp-kit/convo"

// mustConvo panics if convo.New failed. The handler always passes an
// Agent and no override Store — the only two ways it can fail — so an
// error here is a wiring bug, not a runtime condition.
func mustConvo(m *convo.Manager, err error) *convo.Manager {
	if err != nil {
		panic("handler: convo manager: " + err.Error())
	}
	return m
}
