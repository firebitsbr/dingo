package dgamqp

import (
	// standard
	"errors"
	"fmt"
	"sync"

	// open source
	"github.com/streadway/amqp"

	// internal
	"github.com/mission-liao/dingo"
	"github.com/mission-liao/dingo/common"
	"github.com/mission-liao/dingo/transport"
)

type backend struct {
	AmqpConnection

	// store
	stores   *common.HetroRoutines
	rids     map[string]int
	ridsLock sync.Mutex

	// reporter
	reporters *common.HetroRoutines
	cfg       *AmqpConfig
}

func NewBackend(cfg *AmqpConfig) (v *backend, err error) {
	v = &backend{
		reporters: common.NewHetroRoutines(),
		rids:      make(map[string]int),
		cfg:       cfg,
		stores:    common.NewHetroRoutines(),
	}
	err = v.init()
	return
}

func (me *backend) init() (err error) {
	// call parent's Init
	err = me.AmqpConnection.Init(me.cfg.Connection())
	if err != nil {
		return
	}

	// define exchange
	ci, err := me.AmqpConnection.Channel()
	if err != nil {
		return
	}
	defer me.AmqpConnection.ReleaseChannel(ci)

	// init exchange
	err = ci.Channel.ExchangeDeclare(
		"dingo.x.result", // name of exchange
		"direct",         // kind
		true,             // durable
		false,            // auto-delete
		false,            // internal
		false,            // noWait
		nil,              // args
	)
	if err != nil {
		return
	}

	return
}

//
// common.Object interface
//

func (me *backend) Events() ([]<-chan *common.Event, error) {
	return []<-chan *common.Event{
		me.reporters.Events(),
		me.stores.Events(),
	}, nil
}

func (me *backend) Close() (err error) {
	me.reporters.Close()
	me.stores.Close()
	err = me.AmqpConnection.Close()
	return
}

//
// Reporter interface
//

func (me *backend) Report(reports <-chan *dingo.ReportEnvelope) (id int, err error) {
	quit, done, id := me.reporters.New(0)
	go me._reporter_routine_(quit, done, me.reporters.Events(), reports)
	return
}

//
// Store interface
//

func (me *backend) Poll(id transport.Meta) (reports <-chan []byte, err error) {
	// bind to the queue for this task
	tag, qName, rKey := getConsumerTag(id), getQueueName(id), getRoutingKey(id)
	quit, done, idx := me.stores.New(0)

	me.ridsLock.Lock()
	defer me.ridsLock.Unlock()
	me.rids[id.ID()] = idx

	// acquire a free channel
	ci, err := me.AmqpConnection.Channel()
	if err != nil {
		return
	}
	var (
		dv <-chan amqp.Delivery
		r  chan []byte
	)
	defer func() {
		if err != nil {
			me.AmqpConnection.ReleaseChannel(ci)
		} else {
			go me._store_routine_(quit, done, me.stores.Events(), r, ci, dv, id)
		}
	}()
	// declare a queue for this task
	_, err = ci.Channel.QueueDeclare(
		qName, // name of queue
		true,  // durable
		false, // auto-delete
		false, // exclusive
		false, // nowait
		nil,   // args
	)
	if err != nil {
		return
	}

	// bind queue to result-exchange
	err = ci.Channel.QueueBind(
		qName,            // name of queue
		rKey,             // routing key
		"dingo.x.result", // name of exchange
		false,            // nowait
		nil,              // args
	)
	if err != nil {
		return
	}

	dv, err = ci.Channel.Consume(
		qName, // name of queue
		tag,   // consumer Tag
		false, // autoAck
		false, // exclusive
		false, // noLocal
		false, // noWait
		nil,   // args
	)
	if err != nil {
		return
	}

	r = make(chan []byte, 10)
	reports = r
	return
}

func (me *backend) Done(id transport.Meta) (err error) {
	me.ridsLock.Lock()
	defer me.ridsLock.Unlock()

	v, ok := me.rids[id.ID()]
	if !ok {
		err = errors.New("store id not found")
		return
	}

	return me.stores.Stop(v)
}

//
// routine definition
//

func (me *backend) _reporter_routine_(quit <-chan int, done chan<- int, events chan<- *common.Event, reports <-chan *dingo.ReportEnvelope) {
	defer func() {
		done <- 1
	}()

	out := func(e *dingo.ReportEnvelope) (err error) {
		// report an error event when leaving
		defer func() {
			if err != nil {
				events <- common.NewEventFromError(common.InstT.REPORTER, err)
			}
		}()

		// acquire a channel
		ci, err := me.AmqpConnection.Channel()
		if err != nil {
			return
		}
		defer me.AmqpConnection.ReleaseChannel(ci)

		// QueueDeclare and QueueBind should be done in Poll(...)
		err = ci.Channel.Publish(
			"dingo.x.result",    // name of exchange
			getRoutingKey(e.ID), // routing key
			false,               // madatory
			false,               // immediate
			amqp.Publishing{
				DeliveryMode: amqp.Persistent,
				ContentType:  "text/json",
				Body:         e.Body,
			},
		)
		if err != nil {
			return
		}

		// block until amqp.Channel.NotifyPublish
		// TODO: time out, retry
		cf := <-ci.Confirm
		if !cf.Ack {
			err = errors.New("Unable to publish to server")
			return
		}

		return
	}

finished:
	for {
		select {
		case _, _ = <-quit:
			break finished
		case e, ok := <-reports:
			if !ok {
				// reports channel is closed
				break finished
			}
			out(e)
		}
	}

done:
	// cosume all remaining reports
	for {
		select {
		case e, ok := <-reports:
			if !ok {
				break done
			}
			out(e)
		default:
			break done
		}
	}
}

func (me *backend) _store_routine_(
	quit <-chan int,
	done chan<- int,
	events chan<- *common.Event,
	reports chan<- []byte,
	ci *AmqpChannel,
	dv <-chan amqp.Delivery,
	id transport.Meta) {

	var (
		err            error
		isChannelError bool = false
	)

	defer func() {
		if isChannelError {
			// when error occurs, this channel is
			// automatically closed.
			me.AmqpConnection.ReleaseChannel(nil)
		} else {
			me.AmqpConnection.ReleaseChannel(ci)
		}
		done <- 1
		close(done)
		close(reports)
	}()

finished:
	for {
		select {
		case _, _ = <-quit:
			break finished
		case d, ok := <-dv:
			if !ok {
				break finished
			}

			d.Ack(false)
			reports <- d.Body
		}
	}

	// consuming remaining stuffs in queue
done:
	for {
		select {
		case d, ok := <-dv:
			if !ok {
				break done
			}

			d.Ack(false)
			reports <- d.Body
		default:
			break done
		}
	}

	// cancel consuming
	err = ci.Channel.Cancel(getConsumerTag(id), false)
	if err != nil {
		events <- common.NewEventFromError(common.InstT.STORE, err)
		isChannelError = true
		return
	}

	// unbind queue from exchange
	qName, rKey := getQueueName(id), getRoutingKey(id)
	err = ci.Channel.QueueUnbind(
		qName,            // name of queue
		rKey,             // routing key
		"dingo.x.result", // name of exchange
		nil,              // args
	)
	if err != nil {
		events <- common.NewEventFromError(common.InstT.STORE, err)
		isChannelError = true
		return
	}

	// delete queue
	_, err = ci.Channel.QueueDelete(
		qName, // name of queue
		true,  // ifUnused
		true,  // ifEmpty
		false, // noWait
	)
	if err != nil {
		events <- common.NewEventFromError(common.InstT.STORE, err)
		isChannelError = true
		return
	}
}

//
// private function
//

//
func getQueueName(id transport.Meta) string {
	return fmt.Sprintf("dingo.q.%q", id.ID())
}

//
func getRoutingKey(id transport.Meta) string {
	return fmt.Sprintf("dingo.rkey.%q", id.ID())
}

//
func getConsumerTag(id transport.Meta) string {
	return fmt.Sprintf("dingo.consumer.%q", id.ID())
}
