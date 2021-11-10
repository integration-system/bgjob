package bgjob_test

import (
	"context"
	"database/sql"
	"io/ioutil"
	"testing"
	"time"

	"github.com/integration-system/bgjob"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/require"
)

const (
	dsn = "postgres://test:test@localhost:5432/test"
)

func TestClient_Enqueue(t *testing.T) {
	require, db := prepareTest(t)

	cli := bgjob.NewClient(db.DB)
	delay := 5 * time.Second
	req := bgjob.EnqueueRequest{
		Id:    "123",
		Queue: "name",
		Type:  "test",
		Arg:   []byte(`{"simpleJson": 1}`),
		Delay: delay,
	}
	err := cli.Enqueue(context.Background(), req)
	require.NoError(err)

	job, err := getJob(db.DB, "123")
	require.NoError(err)
	require.NotNil(job)
	require.Equal("123", job.Id)
	require.Equal("name", job.Queue)
	require.Equal("test", job.Type)
	require.Equal([]byte(`{"simpleJson": 1}`), job.Arg)
	require.EqualValues(0, job.Attempt)
	require.Nil(job.LastError)
	require.True(time.Now().Unix() <= job.NextRunAt-1)
}

func TestClient_EnqueueConflict(t *testing.T) {
	require, db := prepareTest(t)

	cli := bgjob.NewClient(db.DB)
	delay := 5 * time.Second
	req := bgjob.EnqueueRequest{
		Id:    "123",
		Queue: "name",
		Type:  "test",
		Arg:   []byte(`{"simpleJson": 1}`),
		Delay: delay,
	}
	err := cli.Enqueue(context.Background(), req)
	require.NoError(err)
	err = cli.Enqueue(context.Background(), req)
	require.Error(err)
}

func TestClient_EnqueueGenerateId(t *testing.T) {
	require, db := prepareTest(t)

	cli := bgjob.NewClient(db.DB)
	delay := 5 * time.Second
	req := bgjob.EnqueueRequest{
		Queue: "name",
		Type:  "test",
		Arg:   []byte(`{"simpleJson": 1}`),
		Delay: delay,
	}
	err := cli.Enqueue(context.Background(), req)
	require.NoError(err)
	err = cli.Enqueue(context.Background(), req)
	require.NoError(err)
}

func TestClient_DoEmptyQueue(t *testing.T) {
	require, db := prepareTest(t)
	cli := bgjob.NewClient(db.DB)
	err := cli.Do(context.Background(), "name", func(ctx context.Context, job bgjob.Job) bgjob.Result {
		return bgjob.Complete()
	})
	require.EqualValues(bgjob.ErrEmptyQueue, err)
}

func TestClient_DoComplete(t *testing.T) {
	require, db := prepareTest(t)

	cli := bgjob.NewClient(db.DB)
	req := bgjob.EnqueueRequest{
		Id:    "123",
		Queue: "name",
		Type:  "test",
		Arg:   []byte(`{"simpleJson": 1}`),
	}
	err := cli.Enqueue(context.Background(), req)
	require.NoError(err)
	err = cli.Do(context.Background(), "name", func(ctx context.Context, job bgjob.Job) bgjob.Result {
		require.NoError(err)
		require.NotNil(job)
		require.Equal("123", job.Id)
		require.Equal("name", job.Queue)
		require.Equal("test", job.Type)
		require.Equal([]byte(`{"simpleJson": 1}`), job.Arg)
		require.EqualValues(1, job.Attempt)
		require.Nil(job.LastError)
		return bgjob.Complete()
	})
	require.NoError(err)
	_, err = getJob(db.DB, "123")
	require.True(errors.Is(err, sql.ErrNoRows))
}

func TestClient_DoDelayed(t *testing.T) {
	require, db := prepareTest(t)

	cli := bgjob.NewClient(db.DB)
	req := bgjob.EnqueueRequest{
		Id:    "123",
		Queue: "name",
		Type:  "test",
		Arg:   []byte(`{"simpleJson": 1}`),
		Delay: 3 * time.Second,
	}
	err := cli.Enqueue(context.Background(), req)
	require.NoError(err)

	err = cli.Do(context.Background(), "name", func(ctx context.Context, job bgjob.Job) bgjob.Result {
		return bgjob.Complete()
	})
	require.EqualValues(bgjob.ErrEmptyQueue, err)

	time.Sleep(3 * time.Second)

	err = cli.Do(context.Background(), "name", func(ctx context.Context, job bgjob.Job) bgjob.Result {
		return bgjob.Complete()
	})
	require.NoError(err)
	_, err = getJob(db.DB, "123")
	require.True(errors.Is(err, sql.ErrNoRows))
}

func TestClient_DoRetry(t *testing.T) {
	require, db := prepareTest(t)

	cli := bgjob.NewClient(db.DB)
	req := bgjob.EnqueueRequest{
		Id:    "123",
		Queue: "name",
		Type:  "test",
		Arg:   []byte(`{"simpleJson": 1}`),
	}
	err := cli.Enqueue(context.Background(), req)
	require.NoError(err)
	err = cli.Do(context.Background(), "name", func(ctx context.Context, job bgjob.Job) bgjob.Result {
		return bgjob.Retry(0, errors.New("test error"))
	})
	require.NoError(err)

	err = cli.Do(context.Background(), "name", func(ctx context.Context, job bgjob.Job) bgjob.Result {
		require.EqualValues(2, job.Attempt)
		require.EqualValues("test error", *job.LastError)
		return bgjob.Retry(5*time.Second, errors.New("test error 2"))
	})
	require.NoError(err)

	err = cli.Do(context.Background(), "name", func(ctx context.Context, job bgjob.Job) bgjob.Result {
		return bgjob.Complete()
	})
	require.EqualValues(bgjob.ErrEmptyQueue, err)

	time.Sleep(5 * time.Second)
	err = cli.Do(context.Background(), "name", func(ctx context.Context, job bgjob.Job) bgjob.Result {
		require.EqualValues(3, job.Attempt)
		require.EqualValues("test error 2", *job.LastError)
		return bgjob.Complete()
	})
	require.NoError(err)

	_, err = getJob(db.DB, "123")
	require.True(errors.Is(err, sql.ErrNoRows))
}

func TestClient_DoDlq(t *testing.T) {
	require, db := prepareTest(t)

	cli := bgjob.NewClient(db.DB)
	req := bgjob.EnqueueRequest{
		Id:    "123",
		Queue: "name",
		Type:  "test",
		Arg:   []byte(`{"simpleJson": 1}`),
	}
	err := cli.Enqueue(context.Background(), req)
	require.NoError(err)
	err = cli.Do(context.Background(), "name", func(ctx context.Context, job bgjob.Job) bgjob.Result {
		return bgjob.MoveToDlq(errors.New("test error"))
	})
	require.NoError(err)

	_, err = getJob(db.DB, "123")
	require.True(errors.Is(err, sql.ErrNoRows))

	job, err := getDeadJob(db.DB, "123")
	require.NoError(err)
	require.NotNil(job)
	require.Equal("123", job.Id)
	require.Equal("name", job.Queue)
	require.Equal("test", job.Type)
	require.Equal([]byte(`{"simpleJson": 1}`), job.Arg)
	require.EqualValues(1, job.Attempt)
	require.EqualValues("test error", *job.LastError)
}

func prepareTest(t *testing.T) (*require.Assertions, *db) {
	asserter := require.New(t)
	db, err := Open(dsn, t)
	asserter.NoError(err)
	t.Cleanup(func() {
		_ = db.Close()
	})

	err = applyMigration(db.DB)
	asserter.NoError(err)

	return asserter, db
}

func applyMigration(db *sql.DB) error {
	query, err := ioutil.ReadFile("migration/init.sql")
	if err != nil {
		return errors.WithMessage(err, "read migration")
	}

	_, err = db.Exec(string(query))
	return errors.WithMessage(err, "migration exec")
}

func getJob(db *sql.DB, id string) (*bgjob.Job, error) {
	query := `
SELECT id, queue, type, arg, attempt, last_error, next_run_at, created_at, updated_at
FROM bgjob_job
WHERE id = $1
`
	job := bgjob.Job{}
	err := db.QueryRow(query, id).Scan(
		&job.Id,
		&job.Queue,
		&job.Type,
		&job.Arg,
		&job.Attempt,
		&job.LastError,
		&job.NextRunAt,
		&job.CreatedAt,
		&job.UpdatedAt,
	)
	if err != nil {
		return nil, errors.WithMessage(err, "select job")
	}
	return &job, nil
}

func getDeadJob(db *sql.DB, id string) (*bgjob.Job, error) {
	query := `
SELECT job_id, queue, type, arg, attempt, last_error, next_run_at, job_created_at, job_updated_at
FROM bgjob_dead_job
WHERE job_id = $1
`
	job := bgjob.Job{}
	err := db.QueryRow(query, id).Scan(
		&job.Id,
		&job.Queue,
		&job.Type,
		&job.Arg,
		&job.Attempt,
		&job.LastError,
		&job.NextRunAt,
		&job.CreatedAt,
		&job.UpdatedAt,
	)
	if err != nil {
		return nil, errors.WithMessage(err, "select job")
	}
	return &job, nil
}
