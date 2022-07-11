package memphis

import (
	"errors"
	"time"

	"github.com/nats-io/nats.go"
	log "github.com/sirupsen/logrus"
)

const (
	consumerDefaultPingInterval = 30 * time.Second
)

// Consumer - memphis consumer object.
type Consumer struct {
	Name               string
	ConsumerGroup      string
	PullInterval       time.Duration
	BatchSize          int
	BatchMaxTimeToWait time.Duration
	MaxAckTime         time.Duration
	MaxMsgDeliveries   int
	conn               *Conn
	stationName        string
	subscription       *nats.Subscription
	pingInterval       time.Duration
	subscriptionActive bool
	firstFetch         bool
	consumeActive      bool
	consumeQuit        chan struct{}
	pingQuit           chan struct{}
}

// Msg - a received message, can be acked.
type Msg struct {
	msg *nats.Msg
}

// Msg.Data - get message's data.
func (m *Msg) Data() []byte {
	return m.msg.Data
}

// Msg.Ack - ack the message.
func (m *Msg) Ack() error {
	return m.msg.Ack()
}

type createConsumerReq struct {
	Name             string `json:"name"`
	StationName      string `json:"station_name"`
	ConnectionId     string `json:"connection_id"`
	ConsumerType     string `json:"consumer_type"`
	ConsumerGroup    string `json:"consumers_group"`
	MaxAckTimeMillis int    `json:"max_ack_time_ms"`
	MaxMsgDeliveries int    `json:"max_msg_deliveries"`
}

type removeConsumerReq struct {
	Name        string `json:"name"`
	StationName string `json:"station_name"`
}

// ProduceOpts - configuration options for a consumer.
type ConsumerOpts struct {
	Name               string
	StationName        string
	ConsumerGroup      string
	PullInterval       time.Duration
	BatchSize          int
	BatchMaxTimeToWait time.Duration
	MaxAckTime         time.Duration
	MaxMsgDeliveries   int
}

// GetDefaultConsumerOptions - returns default configuration options for consumers.
func GetDefaultConsumerOptions() ConsumerOpts {
	return ConsumerOpts{
		PullInterval:       1 * time.Second,
		BatchSize:          10,
		BatchMaxTimeToWait: 5 * time.Second,
		MaxAckTime:         30 * time.Second,
		MaxMsgDeliveries:   10,
	}
}

// ConsumerOpt  - a function on the options for consumers.
type ConsumerOpt func(*ConsumerOpts) error

// CreateConsumer - creates a consumer.
func (c *Conn) CreateConsumer(stationName, consumerName string, opts ...ConsumerOpt) (*Consumer, error) {
	defaultOpts := GetDefaultConsumerOptions()

	defaultOpts.Name = consumerName
	defaultOpts.StationName = stationName
	defaultOpts.ConsumerGroup = consumerName

	for _, opt := range opts {
		if opt != nil {
			if err := opt(&defaultOpts); err != nil {
				return nil, err
			}
		}
	}

	return defaultOpts.CreateConsumer(c)
}

// ConsumerOpts.CreateConsumer - creates a consumer using a configuration struct.
func (opts *ConsumerOpts) CreateConsumer(c *Conn) (*Consumer, error) {
	consumer := Consumer{Name: opts.Name,
		ConsumerGroup:      opts.ConsumerGroup,
		PullInterval:       opts.PullInterval,
		BatchSize:          opts.BatchSize,
		MaxAckTime:         opts.MaxAckTime,
		MaxMsgDeliveries:   opts.MaxMsgDeliveries,
		BatchMaxTimeToWait: opts.BatchMaxTimeToWait,
		conn:               c,
		stationName:        opts.StationName}

	err := c.create(&consumer)
	if err != nil {
		return nil, err
	}

	consumer.firstFetch = true
	consumer.consumeQuit = make(chan struct{}, 1)
	consumer.pingQuit = make(chan struct{}, 1)

	consumer.pingInterval = consumerDefaultPingInterval

	subj := consumer.stationName + ".final"

	consumer.subscription, err = c.brokerSubscribe(subj,
		consumer.ConsumerGroup,
		nats.ManualAck(),
		nats.AckWait(consumer.MaxAckTime),
		nats.MaxRequestExpires(consumer.BatchMaxTimeToWait),
		nats.MaxRequestBatch(opts.BatchSize))
	if err != nil {
		return nil, err
	}

	consumer.subscriptionActive = true

	go consumer.pingConsumer()

	return &consumer, err
}

// Station.CreateProducer - creates a producer attached to this station.
func (s *Station) CreateConsumer(name string, opts ...ConsumerOpt) (*Consumer, error) {
	return s.conn.CreateConsumer(s.Name, name, opts...)
}

func (c *Consumer) pingConsumer() {
	ticker := time.NewTicker(c.pingInterval)
	if !c.subscriptionActive {
		log.Fatal("started ping for inactive subscription")
	}

	for {
		select {
		case <-ticker.C:
			_, err := c.subscription.ConsumerInfo()
			if err != nil {
				c.subscriptionActive = false
				log.Error("Station unreachable")
				return
			}
		case <-c.pingQuit:
			ticker.Stop()
			return
		}
	}

}

// ConsumeHandler - handler for consumed messages
type ConsumeHandler func([]*Msg, error)

// Consumer.Consume - start consuming messages according to the interval configured in the consumer object.
// When a batch is consumed the handlerFunc will be called.
func (c *Consumer) Consume(handlerFunc ConsumeHandler) {
	ticker := time.NewTicker(c.PullInterval)

	if c.firstFetch {
		c.firstFetch = false
		msgs, err := c.fetchSubscription()
		go handlerFunc(msgs, err)
	}

	go func() {
		for {
			select {
			case <-ticker.C:
				msgs, err := c.fetchSubscription()
				go handlerFunc(msgs, err)
			case <-c.consumeQuit:
				ticker.Stop()
				return
			}
		}
	}()
	c.consumeActive = true
}

// StopConsume - stops the continuous consume operation.
func (c *Consumer) StopConsume() {
	if !c.consumeActive {
		log.Error("Consume is inactive")
		return
	}
	c.consumeQuit <- struct{}{}
	c.consumeActive = false
}

func (c *Consumer) fetchSubscription() ([]*Msg, error) {
	if !c.subscriptionActive {
		return nil, errors.New("Station unreachable")
	}

	subscription := c.subscription
	batchSize := c.BatchSize
	msgs, err := subscription.Fetch(batchSize)
	if err != nil {
		return nil, err
	}

	wrappedMsgs := make([]*Msg, 0, batchSize)

	for _, msg := range msgs {
		wrappedMsgs = append(wrappedMsgs, &Msg{msg: msg})
	}
	return wrappedMsgs, nil
}

//
type fetchResult struct {
	msgs []*Msg
	err  error
}

func (c *Consumer) fetchSubscriprionWithTimeout() ([]*Msg, error) {
	timeoutDuration := c.BatchMaxTimeToWait
	out := make(chan fetchResult, 1)
	go func() {
		msgs, err := c.fetchSubscription()
		out <- fetchResult{msgs: msgs, err: err}
	}()
	select {
	case <-time.After(timeoutDuration):
		return nil, errors.New("Fetch timed out")
	case fetchRes := <-out:
		return fetchRes.msgs, fetchRes.err

	}
}

// Fetch - immediately fetch a message batch.
func (c *Consumer) Fetch() ([]*Msg, error) {
	if c.firstFetch {
		c.firstFetch = false
	}

	return c.fetchSubscriprionWithTimeout()
}

// Destroy - destoy this consumer.
func (c *Consumer) Destroy() error {
	if c.consumeActive {
		c.StopConsume()
	}
	if c.subscriptionActive {
		c.pingQuit <- struct{}{}
	}

	return c.conn.destroy(c)
}

func (c *Consumer) getCreationApiPath() string {
	return "/api/consumers/createConsumer"
}

func (c *Consumer) getCreationReq() any {
	return createConsumerReq{
		Name:             c.Name,
		StationName:      c.stationName,
		ConnectionId:     c.conn.ConnId,
		ConsumerType:     "application",
		ConsumerGroup:    c.ConsumerGroup,
		MaxAckTimeMillis: int(c.MaxAckTime.Milliseconds()),
		MaxMsgDeliveries: c.MaxMsgDeliveries,
	}
}

func (p *Consumer) getDestructionApiPath() string {
	return "/api/consumers/destroyConsumer"
}

func (p *Consumer) getDestructionReq() any {
	return removeConsumerReq{Name: p.Name, StationName: p.stationName}
}

// ConsumerName - name for the consumer.
func ConsumerName(name string) ConsumerOpt {
	return func(opts *ConsumerOpts) error {
		opts.Name = name
		return nil
	}
}

// StationNameOpt - station name to consume messages from.
func StationNameOpt(stationName string) ConsumerOpt {
	return func(opts *ConsumerOpts) error {
		opts.StationName = stationName
		return nil
	}
}

// ConsumerGroup - consumer group name, default is "".
func ConsumerGroup(cg string) ConsumerOpt {
	return func(opts *ConsumerOpts) error {
		opts.ConsumerGroup = cg
		return nil
	}
}

// PullInterval - interval between pulls, default is 1 second.
func PullInterval(pullInterval time.Duration) ConsumerOpt {
	return func(opts *ConsumerOpts) error {
		opts.PullInterval = pullInterval
		return nil
	}
}

// BatchSize - pull batch size.
func BatchSize(batchSize int) ConsumerOpt {
	return func(opts *ConsumerOpts) error {
		opts.BatchSize = batchSize
		return nil
	}
}

// BatchMaxWaitTime - max time to wait between pulls, defauls is 5 seconds.
func BatchMaxWaitTime(batchMaxWaitTime time.Duration) ConsumerOpt {
	return func(opts *ConsumerOpts) error {
		opts.BatchMaxTimeToWait = batchMaxWaitTime
		return nil
	}
}

// MaxAckTime - max time for ack a message, in case a message not acked within this time period memphis will resend it.
func MaxAckTime(maxAckTime time.Duration) ConsumerOpt {
	return func(opts *ConsumerOpts) error {
		opts.MaxAckTime = maxAckTime
		return nil
	}
}

// MaxMsgDeliveries - max number of message deliveries, by default is 10.
func MaxMsgDeliveries(maxMsgDeliveries int) ConsumerOpt {
	return func(opts *ConsumerOpts) error {
		opts.MaxMsgDeliveries = maxMsgDeliveries
		return nil
	}
}
