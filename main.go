package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/gorilla/websocket"
	"github.com/joho/godotenv"
	"github.com/redis/go-redis/v9"
)

type ChatMessage struct {
	Username string `json:"username"`
	Text     string `json:"text"`
}

type Server struct {
	rdb *redis.Client

	upgrader *websocket.Upgrader

	ops chan func(map[*websocket.Conn]bool)
}

func NewServer(redisURL string) (*Server, error) {
	opt, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, err
	}

	s := &Server{
		rdb: redis.NewClient(opt),

		upgrader: &websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				return true
			},
		},

		ops: make(chan func(map[*websocket.Conn]bool)),
	}

	go s.run()

	return s, nil
}

func (s *Server) HandleConnetions(w http.ResponseWriter, r *http.Request) {
	ws, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Print(err)
		return
	}
	// ensure connection close when function returns
	defer ws.Close()

	s.addClient(ws)
	defer s.delClient(ws)

	for {
		var msg ChatMessage

		// Read in a new message as JSON and map it to a Message object
		err := ws.ReadJSON(&msg)
		if err != nil {
			log.Print(err)
			break
		}

		s.sendMessage(msg)
	}
}

func (s *Server) addClient(ws *websocket.Conn) {
	s.ops <- func(clients map[*websocket.Conn]bool) {
		clients[ws] = true

		// if it's zero, no messages were ever sent/saved
		if s.rdb.Exists(context.Background(), "chat_messages").Val() != 0 {
			s.sendPreviousMessages(ws)
		}
	}
}

func (s *Server) sendPreviousMessages(ws *websocket.Conn) {
	chatMessages, err := s.rdb.LRange(context.Background(), "chat_messages", 0, -1).Result()
	if err != nil {
		log.Print(err)
		return
	}

	// send previous messages
	for _, message := range chatMessages {
		var msg ChatMessage
		_ = json.NewDecoder(strings.NewReader(message)).Decode(&msg)

		err := ws.WriteJSON(msg)
		if err != nil && unsafeError(err) {
			log.Print(err)
			return
		}
	}
}

func (s *Server) delClient(ws *websocket.Conn) {
	s.ops <- func(clients map[*websocket.Conn]bool) {
		delete(clients, ws)
	}
}

func (s *Server) sendMessage(msg ChatMessage) {
	s.ops <- func(clients map[*websocket.Conn]bool) {
		if err := s.storeInRedis(msg); err != nil {
			log.Fatal(err)
		}

		for ws := range clients {
			err := ws.WriteJSON(msg)
			if err != nil && unsafeError(err) {
				log.Print(err)
				ws.Close()
				delete(clients, ws)
			}
		}
	}
}

func (s *Server) storeInRedis(msg ChatMessage) error {
	json, err := json.Marshal(msg)
	if err != nil {
		return err
	}

	err = s.rdb.RPush(context.Background(), "chat_messages", json).Err()
	if err != nil {
		return err
	}

	return nil
}

func (s *Server) run() {
	clients := make(map[*websocket.Conn]bool)

	for op := range s.ops {
		op(clients)
	}
}

func main() {
	err := godotenv.Load()
	if err != nil {
		log.Fatal(err)
	}

	redisURL := os.Getenv("REDIS_URL")
	s, err := NewServer(redisURL)
	if err != nil {
		log.Fatal(err)
	}

	port := os.Getenv("PORT")

	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.Dir("./public")))
	mux.HandleFunc("/websocket", s.HandleConnetions)

	log.Print("Server starting at localhost:" + port)
	_ = http.ListenAndServe(":"+port, mux)
}

// If a message is sent while a client is closing, ignore the error
func unsafeError(err error) bool {
	return !websocket.IsCloseError(err, websocket.CloseGoingAway) && err != io.EOF
}
