package api

import (
	"AMA/internal/store/pgstore"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"

	"github.com/go-chi/chi/middleware"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/cors"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5"
)

type apiHandler struct {
	q          *pgstore.Queries
	r          *chi.Mux
	upgrader   websocket.Upgrader
	subscriber map[string]map[*websocket.Conn]context.CancelFunc
	mu         *sync.Mutex
}

func (h apiHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.r.ServeHTTP(w, r)
}
func Newhandler(q *pgstore.Queries) http.Handler {
	a := apiHandler{
		q:          q,
		upgrader:   websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }},
		subscriber: make((map[string]map[*websocket.Conn]context.CancelFunc)),
		mu:         &sync.Mutex{},
	}

	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.Recoverer, middleware.Logger)

	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   []string{"https://*", "http://*"},
		AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "OPTIONS", "PATCH"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", "X-CSRF-Token"},
		ExposedHeaders:   []string{"Link"},
		AllowCredentials: false,
		MaxAge:           300, // Maximum value not ignored by any of major browsers
	}))

	r.Get("/subscribe/{room_id}", a.handleSubscribe)

	r.Route("/api", func(r chi.Router) {
		r.Route("/rooms", func(r chi.Router) {
			r.Post("/", a.handleCreateRoom)
			r.Get("/", a.handleGetRooms)

			r.Route("/{room_id}/messages", func(r chi.Router) {
				r.Get("/", a.handleGetRoomMessages)
				r.Post("/", a.handleCreateRoomMessages)

				r.Route("/{message_id}", func(r chi.Router) {
					r.Get("/", a.handleGetRoomMessage)
					r.Patch("/react", a.handleReactToMessage)
					r.Delete("/react", a.handleRemoveReactToMessage)
					r.Patch("/answer", a.handleMarkMessageAsAnswered)
				})

			})
		})
	})

	a.r = r

	return a
}

const (
	MessageKindMessageCreated = "message_created"
)

type Message struct {
	Kind   string `json:"kind"`
	Value  any    `json:"value"`
	RoomID string `json:"-"`
}

type MessageMessageCreated struct {
	ID      string `json:"id"`
	Message string `json:"message"`
}

func (h apiHandler) notifyClients(msg Message) {

	h.mu.Lock()
	defer h.mu.Unlock()

	subscribers, ok := h.subscriber[msg.RoomID]

	if !ok || len(subscribers) == 0 {
		return
	}

	for conn, cancel := range subscribers {
		if err := conn.WriteJSON(msg); err != nil {
			slog.Error("failed to send message to client", "error", err)
			cancel()
		}
	}
}
func (h apiHandler) handleCreateRoom(w http.ResponseWriter, r *http.Request) {

	type _body struct {
		Theme string `json:"theme"`
	}

	var body _body

	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	roomID, err := h.q.InsertRoom(r.Context(), body.Theme)

	if err != nil {

		slog.Error("Failed to insert room", "error", err)
		http.Error(w, "something went wrong", http.StatusInternalServerError)

		return
	}

	type response struct {
		ID string `json:"id"`
	}

	data, _ := json.Marshal(response{ID: roomID.String()})

	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}
func (h apiHandler) handleGetRooms(w http.ResponseWriter, r *http.Request) {}
func (h apiHandler) handleGetRoomMessages(w http.ResponseWriter, r *http.Request) {

}
func (h apiHandler) handleCreateRoomMessages(w http.ResponseWriter, r *http.Request) {

	rawRoomId := chi.URLParam(r, "room_id")
	roomId, err := uuid.Parse(rawRoomId)

	if err != nil {
		http.Error(w, "invalid room id", http.StatusBadRequest)
		return
	}

	_, err = h.q.GetRoom(r.Context(), roomId)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "room not found", http.StatusBadRequest)
			return
		}

		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}

	type _body struct {
		Message string `json:"message"`
	}

	var body _body

	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	messageId, err := h.q.InsertMessage(r.Context(), pgstore.InsertMessageParams{RoomID: roomId, Message: body.Message})

	if err != nil {
		slog.Error("failed to insert message", "error", err)
		http.Error(w, "something went wrong", http.StatusInternalServerError)
	}

	type response struct {
		ID string `json:"id"`
	}

	data, _ := json.Marshal(response{ID: string(messageId.String())})

	w.Header().Set("Content-Type", "application/josn")
	_, _ = w.Write(data)

	go h.notifyClients(Message{
		Kind:   MessageKindMessageCreated,
		RoomID: rawRoomId,
		Value: MessageMessageCreated{
			ID:      messageId.String(),
			Message: body.Message,
		},
	})
}
func (h apiHandler) handleGetRoomMessage(w http.ResponseWriter, r *http.Request)        {}
func (h apiHandler) handleReactToMessage(w http.ResponseWriter, r *http.Request)        {}
func (h apiHandler) handleRemoveReactToMessage(w http.ResponseWriter, r *http.Request)  {}
func (h apiHandler) handleMarkMessageAsAnswered(w http.ResponseWriter, r *http.Request) {}
func (h apiHandler) handleSubscribe(w http.ResponseWriter, r *http.Request) {

	rawRoomId := chi.URLParam(r, "room_id")
	roomId, err := uuid.Parse(rawRoomId)

	if err != nil {
		http.Error(w, "invalid room id", http.StatusBadRequest)
		return
	}

	_, err = h.q.GetRoom(r.Context(), roomId)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "room not found", http.StatusBadRequest)
			return
		}

		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}

	c, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Warn("Failed to upgrade connection", "error", err)
		http.Error(w, "failed to upgrade to ws connection", http.StatusBadRequest)
	}

	defer c.Close()

	ctx, cancel := context.WithCancel(r.Context())

	h.mu.Lock()
	if _, ok := h.subscriber[rawRoomId]; !ok {
		h.subscriber[rawRoomId] = make(map[*websocket.Conn]context.CancelFunc)
	}
	slog.Info("new client connected", "room_id", rawRoomId, "client_ip", r.RemoteAddr)
	h.subscriber[rawRoomId][c] = cancel

	h.mu.Unlock()

	<-ctx.Done()

	h.mu.Lock()

	delete(h.subscriber[rawRoomId], c)
	h.mu.Unlock()
}
