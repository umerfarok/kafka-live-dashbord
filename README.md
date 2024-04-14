# kafka-live-view
# Create topic 
kafka-topics.sh --create --topic test-topic --bootstrap-server localhost:9092 --partitions 1 --replication-factor 1


# Send messages 
kafka-console-producer.sh --topic test-topic --bootstrap-server localhost:9092

kafka-console-consumer.sh --topic test-topic --bootstrap-server localhost:9092 --from-beginning