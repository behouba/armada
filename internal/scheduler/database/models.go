// Code generated by sqlc. DO NOT EDIT.
// versions:
//   sqlc v1.16.0

package database

import (
	"time"

	"github.com/google/uuid"
)

type Job struct {
	JobID           string    `db:"job_id"`
	JobSet          string    `db:"job_set"`
	Queue           string    `db:"queue"`
	UserID          string    `db:"user_id"`
	Submitted       int64     `db:"submitted"`
	Groups          []byte    `db:"groups"`
	Priority        int64     `db:"priority"`
	CancelRequested bool      `db:"cancel_requested"`
	Cancelled       bool      `db:"cancelled"`
	Succeeded       bool      `db:"succeeded"`
	Failed          bool      `db:"failed"`
	SubmitMessage   []byte    `db:"submit_message"`
	SchedulingInfo  []byte    `db:"scheduling_info"`
	Serial          int64     `db:"serial"`
	LastModified    time.Time `db:"last_modified"`
}

type JobRunAssignment struct {
	RunID        uuid.UUID `db:"run_id"`
	Assignment   []byte    `db:"assignment"`
	Serial       int64     `db:"serial"`
	LastModified time.Time `db:"last_modified"`
}

type Nodeinfo struct {
	ExecutorNodeName string    `db:"executor_node_name"`
	NodeName         string    `db:"node_name"`
	Executor         string    `db:"executor"`
	Message          []byte    `db:"message"`
	Serial           int64     `db:"serial"`
	LastModified     time.Time `db:"last_modified"`
}

type Queue struct {
	Name   string  `db:"name"`
	Weight float64 `db:"weight"`
}

type Run struct {
	RunID          uuid.UUID `db:"run_id"`
	JobID          string    `db:"job_id"`
	JobSet         string    `db:"job_set"`
	Executor       string    `db:"executor"`
	SentToExecutor bool      `db:"sent_to_executor"`
	Cancelled      bool      `db:"cancelled"`
	Running        bool      `db:"running"`
	Succeeded      bool      `db:"succeeded"`
	Failed         bool      `db:"failed"`
	Returned       bool      `db:"returned"`
	Serial         int64     `db:"serial"`
	LastModified   time.Time `db:"last_modified"`
}
