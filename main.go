package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/IBM/sarama"
	"github.com/gorilla/websocket"
	"github.com/samuel/go-zookeeper/zk"
	"github.com/umerfarok/kafka-live-dashboard/config"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

type TopicStatus struct {
	Name        string
	Partitions  int
	Replication int
	Active      bool
	Messages    int64
	Lag         int64
	Throughput  float64
}

type ClusterStatus struct {
	Topics       []TopicStatus
	TotalTopics  int
	ActiveTopics int
	Partitions   int
	Brokers      []BrokerInfo
}

type BrokerInfo struct {
	ID       int32
	Hostname string
	Port     int32
}

type Server struct {
	config        *config.Config
	kafkaConn     sarama.Client
	zkConn        *zk.Conn
	clusterStatus *ClusterStatus
	mu            sync.Mutex
}

func NewServer(config *config.Config) (*Server, error) {
	kafkaConfig := sarama.NewConfig()
	kafkaConfig.Version = sarama.V2_6_0_0
	kafkaConn, err := sarama.NewClient(strings.Split(config.KafkaBrokers, ","), kafkaConfig)
	if err != nil {
		return nil, err
	}

	zkConn, _, err := zk.Connect(strings.Split(config.ZookeeperNodes, ","), time.Second)
	if err != nil {
		return nil, err
	}

	return &Server{
		config:    config,
		kafkaConn: kafkaConn,
		zkConn:    zkConn,
	}, nil
}


func (s *Server) startTopicRefresher() {
	ticker := time.NewTicker(5 * time.Minute)
	go func() {
		for {
			select {
			case <-ticker.C:
				s.updateClusterStatus()
			}
		}
	}()
}


func (s *Server) serveTopicMetrics(w http.ResponseWriter, r *http.Request, topicName string) {
	partitions, replication, active, messages, lag, throughput, err := s.getTopicMetrics(topicName)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to get metrics for topic %s: %v", topicName, err), http.StatusInternalServerError)
		return
	}

	topicMetrics := struct {
		Partitions  int
		Replication int
		Active      bool
		Messages    int64
		Lag         int64
		Throughput  float64
	}{
		Partitions:  partitions,
		Replication: replication,
		Active:      active,
		Messages:    messages,
		Lag:         lag,
		Throughput:  throughput,
	}

	jsonBytes, err := json.Marshal(topicMetrics)
	if err != nil {
		http.Error(w, "Failed to marshal topic metrics", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(jsonBytes)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS, PUT, DELETE")
	w.Header().Set("Access-Control-Allow-Headers", "Accept, Content-Type, Content-Length, Accept-Encoding, X-CSRF-Token, Authorization")

	if r.Method == "OPTIONS" {
		w.WriteHeader(http.StatusOK)
		return
	}

	if r.URL.Path == "/" {
		s.serveClusterStatus(w, r)
		return
	} else if r.URL.Path == "/topics" {
		s.serveTopicList(w, r)
		return
	} else if strings.HasPrefix(r.URL.Path, "/topics/") {
		topicName := strings.TrimPrefix(r.URL.Path, "/topics/")
		s.serveTopicMetrics(w, r, topicName)
		return
	} else if r.URL.Path == "/ws" {
		s.serveWebSocket(w, r)
		return
	}

	http.NotFound(w, r)
}

func (s *Server) serveClusterStatus(w http.ResponseWriter, r *http.Request) {
	s.updateClusterStatus()
	jsonBytes, err := json.Marshal(s.clusterStatus)
	if err != nil {
		http.Error(w, "Failed to marshal cluster status", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(jsonBytes)
}

func (s *Server) serveTopicList(w http.ResponseWriter, r *http.Request) {
	s.updateClusterStatus()
	jsonBytes, err := json.Marshal(s.clusterStatus.Topics)
	if err != nil {
		http.Error(w, "Failed to marshal topic list", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(jsonBytes)
}

func (s *Server) serveWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("WebSocket upgrade error:", err)
		return
	}
	defer conn.Close()

	topic := r.URL.Query().Get("topic")
	if topic == "" {
		http.Error(w, "Topic not specified", http.StatusBadRequest)
		return
	}

	s.handleWebSocket(conn, topic)
}

func (s *Server) updateClusterStatus() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.clusterStatus == nil {
		s.clusterStatus = &ClusterStatus{}
		s.fetchClusterMetadata()
	}
}
func (s *Server) fetchClusterMetadata() {
	topics, err := s.getTopics()
	if err != nil {
		log.Println("Failed to get topics:", err)
		return
	}

	brokers, err := s.getBrokers()
	if err != nil {
		log.Println("Failed to get brokers:", err)
		return
	}

	var wg sync.WaitGroup
	topicStatus := make([]TopicStatus, len(topics))
	totalPartitions := 0
	activeTopics := 0

	for i, topic := range topics {
		wg.Add(1)
		go func(i int, topic string) {
			defer wg.Done()
			partitions, replication, active, messages, lag, throughput, err := s.getTopicMetrics(topic)
			if err != nil {
				log.Printf("Failed to get metrics for topic %s: %v", topic, err)
				return
			}
			topicStatus[i] = TopicStatus{
				Name:        topic,
				Partitions:  partitions,
				Replication: replication,
				Active:      active,
				Messages:    messages,
				Lag:         lag,
				Throughput:  throughput,
			}
			totalPartitions += partitions
			if active {
				activeTopics++
			}
		}(i, topic)
	}

	wg.Wait()

	s.clusterStatus.Topics = topicStatus
	s.clusterStatus.TotalTopics = len(topics)
	s.clusterStatus.ActiveTopics = activeTopics
	s.clusterStatus.Partitions = totalPartitions
	s.clusterStatus.Brokers = brokers
}

func (s *Server) getTopics() ([]string, error) {
	children, _, err := s.zkConn.Children("/brokers/topics")
	if err != nil {
		return nil, err
	}
	return children, nil
}

func (s *Server) getBrokers() ([]BrokerInfo, error) {
	brokerIDs, _, err := s.zkConn.Children("/brokers/ids")
	if err != nil {
		return nil, err
	}

	var brokers []BrokerInfo
	for _, brokerID := range brokerIDs {
		data, _, err := s.zkConn.Get(fmt.Sprintf("/brokers/ids/%s", brokerID))
		if err != nil {
			return nil, err
		}

		var broker struct {
			Timestamp string   `json:"timestamp"`
			Endpoints []string `json:"endpoints"`
			Host      string   `json:"host"`
			Port      int32    `json:"port"`
			Version   int32    `json:"version"`
		}
		if err := json.Unmarshal(data, &broker); err != nil {
			return nil, err
		}

		brokers = append(brokers, BrokerInfo{
			ID:       int32(mustAtoi(brokerID)),
			Hostname: broker.Host,
			Port:     broker.Port,
		})
	}

	return brokers, nil
}

func (s *Server) getTopicMetrics(topic string) (int, int, bool, int64, int64, float64, error) {
	var wg sync.WaitGroup
	wg.Add(3)

	var partitions int
	var replication int
	var active bool
	var messages int64
	var lag int64
	var throughput float64
	var err error

	go func() {
		defer wg.Done()
		partitions, err = s.getPartitionCount(topic)
	}()

	go func() {
		defer wg.Done()
		replication, err = s.getReplicationFactor(topic)
	}()

	go func() {
		defer wg.Done()
		active, messages, lag, throughput, err = s.getTopicActivityMetrics(topic)
	}()

	wg.Wait()

	if err != nil {
		return 0, 0, false, 0, 0, 0, err
	}

	return partitions, replication, active, messages, lag, throughput, nil
}

func (s *Server) getPartitionCount(topic string) (int, error) {
	partitions, _, err := s.zkConn.Children(fmt.Sprintf("/brokers/topics/%s/partitions", topic))
	if err != nil {
		return 0, err
	}
	return len(partitions), nil
}

func (s *Server) getReplicationFactor(topic string) (int, error) {
	data, _, err := s.zkConn.Get(fmt.Sprintf("/brokers/topics/%s", topic))
	if err != nil {
		return 0, err
	}

	var topicInfo struct {
		Partitions map[string][]int32 `json:"partitions"`
	}
	if err := json.Unmarshal(data, &topicInfo); err != nil {
		return 0, err
	}

	// Assume all partitions have the same replication factor
	if len(topicInfo.Partitions) > 0 {
		return len(topicInfo.Partitions["0"]), nil
	}
	return 0, nil
}

func (s *Server) getTopicActivityMetrics(topic string) (bool, int64, int64, float64, error) {
	consumer, err := sarama.NewConsumerFromClient(s.kafkaConn)
	if err != nil {
		return false, 0, 0, 0, err
	}
	defer consumer.Close()

	partitionConsumer, err := consumer.ConsumePartition(topic, 0, sarama.OffsetNewest)
	if err != nil {
		return false, 0, 0, 0, err
	}
	defer partitionConsumer.Close()

	var totalMessages int64
	var totalLag int64
	var totalMessages10s int64
	for i := 0; i < 10; i++ {
		select {
		case message := <-partitionConsumer.Messages():
			totalMessages++
			totalLag += message.Offset
		case <-time.After(1 * time.Second):
			totalMessages10s = totalMessages
			totalMessages = 0
			break
		}
	}

	return totalMessages10s > 0, totalMessages, totalLag, float64(totalMessages10s) / 10.0, nil
}
func (s *Server) handleWebSocket(conn *websocket.Conn, topic string) {
	consumer, err := sarama.NewConsumerFromClient(s.kafkaConn)
	if err != nil {
		log.Println("Failed to create consumer:", err)
		return
	}
	defer consumer.Close()

	partitionConsumer, err := consumer.ConsumePartition(topic, 0, sarama.OffsetNewest)
	if err != nil {
		log.Println("Failed to start consumer for partition:", err)
		return
	}
	defer partitionConsumer.Close()

	done := make(chan struct{})
	defer close(done)

	go func() {
		for {
			select {
			case message := <-partitionConsumer.Messages():
				err := conn.WriteMessage(websocket.TextMessage, message.Value)
				if err != nil {
					log.Println("WebSocket write error:", err)
					return
				}
			case <-done:
				return
			}
		}
	}()
	_, _, err = conn.ReadMessage()
	if err != nil {
		if !websocket.IsCloseError(err, websocket.CloseNormalClosure) {
			log.Println("WebSocket read error:", err)
		}
	}
}
func mustAtoi(s string) int {
	i, err := strconv.Atoi(s)
	if err != nil {
		panic(err)
	}
	return i
}

func main() {
	config, err := config.NewConfig()
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	server, err := NewServer(config)
	server.startTopicRefresher()
	if err != nil {
		log.Fatalf("Failed to create server: %v", err)
	}

	http.Handle("/", server)
	log.Printf("Starting server on :%s\n", config.HTTPPort)
	if err := http.ListenAndServe(":"+config.HTTPPort, nil); err != nil {
		log.Fatalf("ListenAndServe error: %v", err)
	}
}
