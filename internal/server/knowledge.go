package server

import (
	"net/http"

	"diesel/internal/knowledge"

	"github.com/gin-gonic/gin"
)

// SetKnowledge wires the knowledge-graph store into the server so the web UI
// (and any HTTP client) can read and edit the same graph the model uses. Call
// once at startup before Apply; nil leaves the /knowledge routes returning 501.
func (m *Manager) SetKnowledge(store *knowledge.Store) {
	m.mu.Lock()
	m.knStore = store
	m.mu.Unlock()
}

// knowledgeStore returns the wired store under the lock, or nil.
func (m *Manager) knowledgeStore() *knowledge.Store {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.knStore
}

// registerKnowledgeRoutes adds the graph CRUD endpoints to the authed API
// group. Mutating routes use POST (including the /delete variants) so request
// bodies travel reliably across clients that balk at DELETE-with-body.
func (m *Manager) registerKnowledgeRoutes(api *gin.RouterGroup) {
	api.GET("/knowledge", m.handleKnowledgeRead)
	api.POST("/knowledge/entities", m.handleKnowledgeCreateEntities)
	api.POST("/knowledge/entities/delete", m.handleKnowledgeDeleteEntities)
	api.POST("/knowledge/observations", m.handleKnowledgeAddObservations)
	api.POST("/knowledge/observations/delete", m.handleKnowledgeDeleteObservations)
	api.POST("/knowledge/relations", m.handleKnowledgeCreateRelations)
	api.POST("/knowledge/relations/delete", m.handleKnowledgeDeleteRelations)
}

// knStoreOr503 fetches the store or writes a 501 and returns nil — the graph
// routes are only live once SetKnowledge has wired a store.
func (m *Manager) knStoreOr503(c *gin.Context) *knowledge.Store {
	store := m.knowledgeStore()
	if store == nil {
		c.JSON(http.StatusNotImplemented, gin.H{"error": "knowledge graph not available"})
		return nil
	}
	return store
}

func (m *Manager) handleKnowledgeRead(c *gin.Context) {
	store := m.knStoreOr503(c)
	if store == nil {
		return
	}
	g, err := store.ReadGraph(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, g)
}

func (m *Manager) handleKnowledgeCreateEntities(c *gin.Context) {
	store := m.knStoreOr503(c)
	if store == nil {
		return
	}
	var body struct {
		Entities []knowledge.Entity `json:"entities"`
	}
	if err := c.BindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	created, err := store.CreateEntities(c.Request.Context(), body.Entities)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"entities": created})
}

func (m *Manager) handleKnowledgeDeleteEntities(c *gin.Context) {
	store := m.knStoreOr503(c)
	if store == nil {
		return
	}
	var body struct {
		Names []string `json:"names"`
	}
	if err := c.BindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := store.DeleteEntities(c.Request.Context(), body.Names); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (m *Manager) handleKnowledgeAddObservations(c *gin.Context) {
	store := m.knStoreOr503(c)
	if store == nil {
		return
	}
	var body struct {
		Observations []knowledge.ObservationMutation `json:"observations"`
	}
	if err := c.BindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := store.AddObservations(c.Request.Context(), body.Observations); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (m *Manager) handleKnowledgeDeleteObservations(c *gin.Context) {
	store := m.knStoreOr503(c)
	if store == nil {
		return
	}
	var body struct {
		Deletions []knowledge.ObservationMutation `json:"deletions"`
	}
	if err := c.BindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := store.DeleteObservations(c.Request.Context(), body.Deletions); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (m *Manager) handleKnowledgeCreateRelations(c *gin.Context) {
	store := m.knStoreOr503(c)
	if store == nil {
		return
	}
	var body struct {
		Relations []knowledge.Relation `json:"relations"`
	}
	if err := c.BindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	created, err := store.CreateRelations(c.Request.Context(), body.Relations)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"relations": created})
}

func (m *Manager) handleKnowledgeDeleteRelations(c *gin.Context) {
	store := m.knStoreOr503(c)
	if store == nil {
		return
	}
	var body struct {
		Relations []knowledge.Relation `json:"relations"`
	}
	if err := c.BindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := store.DeleteRelations(c.Request.Context(), body.Relations); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
