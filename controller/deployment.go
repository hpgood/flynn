package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/flynn/flynn/Godeps/_workspace/src/github.com/bgentry/que-go"
	"github.com/flynn/flynn/Godeps/_workspace/src/github.com/flynn/go-sql"
	"github.com/flynn/flynn/Godeps/_workspace/src/github.com/flynn/pq"
	"github.com/flynn/flynn/Godeps/_workspace/src/github.com/jackc/pgx"
	ct "github.com/flynn/flynn/controller/types"
	"github.com/flynn/flynn/pkg/postgres"
	"github.com/flynn/flynn/pkg/random"
)

type DeploymentRepo struct {
	db *postgres.DB
	q  *que.Client
}

func NewDeploymentRepo(db *postgres.DB, pgxpool *pgx.ConnPool) *DeploymentRepo {
	q := que.NewClient(pgxpool)
	return &DeploymentRepo{db: db, q: q}
}

func (r *DeploymentRepo) Add(data interface{}) error {
	deployment := data.(*ct.Deployment)
	if deployment.ID == "" {
		deployment.ID = random.UUID()
	}
	query := "INSERT INTO deployments (deployment_id, app_id, old_release_id, new_release_id, strategy) VALUES ($1, $2, $3, $4, $5) RETURNING created_at"
	if err := r.db.QueryRow(query, deployment.ID, deployment.AppID, deployment.OldReleaseID, deployment.NewReleaseID, deployment.Strategy).Scan(&deployment.CreatedAt); err != nil {
		return err
	}
	deployment.ID = postgres.CleanUUID(deployment.ID)
	deployment.OldReleaseID = postgres.CleanUUID(deployment.OldReleaseID)
	deployment.NewReleaseID = postgres.CleanUUID(deployment.NewReleaseID)

	args, err := json.Marshal(ct.DeployID{ID: deployment.ID})
	if err != nil {
		return err
	}
	if err := r.q.Enqueue(&que.Job{
		Type: "Deployment",
		Args: args,
	}); err != nil {
		return err
	}
	return nil
}

func (r *DeploymentRepo) Get(id string) (*ct.Deployment, error) {
	query := "SELECT deployment_id, app_id, old_release_id, new_release_id, strategy, created_at, finished_at FROM deployments WHERE deployment_id = $1"
	row := r.db.QueryRow(query, id)
	return scanDeployment(row)
}

func scanDeployment(s postgres.Scanner) (*ct.Deployment, error) {
	d := &ct.Deployment{}
	err := s.Scan(&d.ID, &d.AppID, &d.OldReleaseID, &d.NewReleaseID, &d.Strategy, &d.CreatedAt, &d.FinishedAt)
	if err == sql.ErrNoRows {
		err = ErrNotFound
	}
	d.ID = postgres.CleanUUID(d.ID)
	d.OldReleaseID = postgres.CleanUUID(d.OldReleaseID)
	d.NewReleaseID = postgres.CleanUUID(d.NewReleaseID)
	return d, err
}

// Deployment events

// TODO: share with controller streamJobs
func streamDeploymentEvents(deploymentID string, w http.ResponseWriter, repo *DeploymentRepo) {
	var err error
	defer func() {
		if err != nil {
			http.Error(w, err.Error(), 500)
		}
	}()

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")

	sendKeepAlive := func() error {
		if _, err := w.Write([]byte(":\n")); err != nil {
			return err
		}
		w.(http.Flusher).Flush()
		return nil
	}

	sendDeploymentEvent := func(e *ct.DeploymentEvent) error {
		if _, err := fmt.Fprintf(w, "id: %d\ndata: ", e.ID); err != nil {
			return err
		}
		if err := json.NewEncoder(w).Encode(e); err != nil {
			return err
		}
		if _, err := w.Write([]byte("\n")); err != nil {
			return err
		}
		w.(http.Flusher).Flush()
		return nil
	}

	connected := make(chan struct{})
	done := make(chan struct{})
	listenEvent := func(ev pq.ListenerEventType, listenErr error) {
		switch ev {
		case pq.ListenerEventConnected:
			close(connected)
		case pq.ListenerEventDisconnected:
			close(done)
		case pq.ListenerEventConnectionAttemptFailed:
			err = listenErr
			close(done)
		}
	}
	listener := pq.NewListener(repo.db.DSN(), 10*time.Second, time.Minute, listenEvent)
	defer listener.Close()
	listener.Listen("deployment_events:" + postgres.FormatUUID(deploymentID))

	var currID int64
	events, err := repo.listEvents(deploymentID, 0)
	if err != nil {
		return
	}
	for _, e := range events {
		if err = sendDeploymentEvent(e); err != nil {
			return
		}
		currID = e.ID
	}

	select {
	case <-done:
		return
	case <-connected:
	}

	if err = sendKeepAlive(); err != nil {
		return
	}

	closed := w.(http.CloseNotifier).CloseNotify()
	for {
		select {
		case <-done:
			return
		case <-closed:
			return
		case <-time.After(30 * time.Second):
			if err = sendKeepAlive(); err != nil {
				return
			}
		case n := <-listener.Notify:
			id, err := strconv.ParseInt(n.Extra, 10, 64)
			if err != nil {
				return
			}
			if id <= currID {
				continue
			}
			e, err := repo.getEvent(id)
			if err != nil {
				return
			}
			if err = sendDeploymentEvent(e); err != nil {
				return
			}
		}
	}
}

func (r *DeploymentRepo) listEvents(deploymentID string, sinceID int64) ([]*ct.DeploymentEvent, error) {
	query := "SELECT event_id, deployment_id, release_id, job_type, job_state, status, created_at FROM deployment_events WHERE deployment_id = $1 AND event_id > $2"
	rows, err := r.db.Query(query, deploymentID, sinceID)
	if err != nil {
		return nil, err
	}
	var events []*ct.DeploymentEvent
	for rows.Next() {
		event, err := scanDeploymentEvent(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		events = append(events, event)
	}
	return events, nil
}

func (r *DeploymentRepo) getEvent(id int64) (*ct.DeploymentEvent, error) {
	row := r.db.QueryRow("SELECT event_id, deployment_id, release_id, job_type, job_state, status, created_at FROM deployment_events WHERE event_id = $1", id)
	return scanDeploymentEvent(row)
}

func scanDeploymentEvent(s postgres.Scanner) (*ct.DeploymentEvent, error) {
	event := &ct.DeploymentEvent{}
	err := s.Scan(&event.ID, &event.DeploymentID, &event.ReleaseID, &event.JobType, &event.JobState, &event.Status, &event.CreatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			err = ErrNotFound
		}
		return nil, err
	}
	event.DeploymentID = postgres.CleanUUID(event.DeploymentID)
	event.ReleaseID = postgres.CleanUUID(event.ReleaseID)
	return event, nil
}
