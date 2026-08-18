package httpapi

import (
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"
)

// respondError writes the API's error envelope and stops the chain.
func respondError(c *gin.Context, status int, message string) {
	c.AbortWithStatusJSON(status, gin.H{"error": message})
}

// NewRouter builds the Gin engine. gin.New (not gin.Default) so request logging
// goes through slog instead of Gin's unstructured logger.
func NewRouter(h *NoteHandler, log *slog.Logger) *gin.Engine {
	r := gin.New()
	r.MaxMultipartMemory = 8 << 20 // 8 MiB in memory, larger uploads spill to disk
	r.Use(RequestID(), RequestLogger(log), Recovery(log))

	r.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})
	r.POST("/v1/notes", h.Create)
	r.GET("/v1/notes/:id", h.Show)

	return r
}
