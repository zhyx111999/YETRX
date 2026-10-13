package server

import (
	"log"

	"github.com/Autumn-27/artex/agent"
)

// activePersonaPrompt is evaluated by ChatAgent at each request boundary. It is
// deliberately not copied into Server's LLM/profile caches: changing the active
// persona should affect the next main or workflow child turn immediately.
func (s *Server) activePersonaPrompt() string {
	if s == nil || s.m == nil || s.m.pg == nil {
		return ""
	}
	persona, err := s.m.pg.ActivePersona()
	if err != nil {
		log.Printf("[persona] load active persona: %v", err)
		return ""
	}
	if persona == nil {
		return ""
	}
	return persona.Prompt
}

func (s *Server) wirePersona(ca *agent.ChatAgent) {
	if ca != nil {
		ca.SetPersonaResolver(s.activePersonaPrompt)
	}
}
