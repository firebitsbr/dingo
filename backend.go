package dingo

import (
	"github.com/mission-liao/dingo/transport"
)

type Backend interface {
	Reporter
	Store
}

type ReportEnvelope struct {
	ID   transport.Meta
	Body []byte
}

// TODO: add test case for consecutive calling to Reporter.Report, and make sure their order wouldn't be wrong.

/*
 Reporter(s) is responsible for sending reports to backend(s). The interaction between
 Reporter(s) and dingo are asynchronous by channels.
*/
type Reporter interface {
	// attach a report channel to backend.
	//
	// parameters:
	// - reports: a input channel to receive reports from dingo.
	// returns:
	// - err: errors
	Report(reports <-chan *ReportEnvelope) (id int, err error)
}

/*
 Store(s) is responsible for receiving reports from backend(s)
*/
type Store interface {
	// polling reports for tasks
	//
	// parameters:
	// - id: the meta info of that task to be polled.
	// returns:
	// - reports: the output channel for dingo to receive reports.
	Poll(id transport.Meta) (reports <-chan []byte, err error)

	// Stop monitoring that task
	//
	// parameters:
	// - id the meta info of that task/report to stop polling.
	Done(id transport.Meta) error
}
