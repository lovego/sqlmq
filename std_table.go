package sqlmq

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	statusWait    = ""
	statusDone    = "done"
	statusGivenUp = "givenUp"

	rfc3339Micro = "2006-01-02T15:04:05.999999Z07:00"
)

type StdMessage struct {
	Id        int64
	Queue     string
	Data      interface{}
	Status    string
	CreatedAt time.Time
	TryCount  uint16
	RetryAt   time.Time `json:",omitempty"`
}

func (msg *StdMessage) QueueName() string {
	return msg.Queue
}

func (msg *StdMessage) ConsumeAt() time.Time {
	return msg.RetryAt
}

func StdTable(db *sql.DB, name string) Table {
	var createSql = fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS %s (
	id            bigserial    NOT NULL PRIMARY KEY,
	queue         text         NOT NULL,
	status        text         NOT NULL,
	created_at    timestamptz  NOT NULL,
	try_count     smallint     NOT NULL,
	retry_at      timestamptz  NOT NULL,
	data          jsonb        NOT NULL
);
CREATE INDEX IF NOT EXISTS %s_retry_at ON %s (retry_at)
WHERE status = '%s'
`, name, name, name, statusWait,
	)
	if _, err := db.Exec(createSql); err != nil {
		log.Panic(err)
	}
	return &stdTable{name: name}
}

type stdTable struct {
	name               string
	queues             []string
	earliestMessageSql string
	mutex              sync.RWMutex
}

func (table *stdTable) SetQueues(queues []string) {
	table.mutex.Lock()
	defer table.mutex.Unlock()
	table.queues = queues
	table.earliestMessageSql = ""
}

func (table *stdTable) EarliestMessage(tx *sql.Tx) (Message, error) {
	row := StdMessage{}
	querysql := table.getEarliestMessageSql()
	if err := tx.QueryRow(querysql).Scan(
		&row.Id, &row.Queue, &row.Data, &row.Status, &row.CreatedAt, &row.TryCount, &row.RetryAt,
	); err == sql.ErrNoRows {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	return &row, nil
}

func (table *stdTable) getEarliestMessageSql() string {
	table.mutex.RLock()
	if table.earliestMessageSql == "" {
		var queues []string
		for _, queue := range table.queues {
			queues = append(queues, quote(queue))
		}
		sort.Strings(queues)
		querySql := fmt.Sprintf(`
		SELECT id, queue, data, status, created_at, try_count, retry_at
		FROM %s
		WHERE queue IN (%s) AND status = '%s'
		ORDER BY retry_at
		LIMIT 1
		FOR UPDATE SKIP LOCKED
		`,
			table.name, strings.Join(queues, ","), statusWait,
		)
		table.mutex.RUnlock()

		table.mutex.Lock()
		table.earliestMessageSql = querySql
		table.mutex.Unlock()

		return querySql
	}
	defer table.mutex.RUnlock()
	return table.earliestMessageSql
}

func (table *stdTable) MarkSuccess(tx *sql.Tx, msg Message) error {
	sql := fmt.Sprintf(`
	UPDATE %s
	SET status = '%s', try_count = try_count+1, retry_at = '%s'
	WHERE id = %d
	`,
		table.name,
		statusDone, time.Now().Format(rfc3339Micro),
		msg.(*StdMessage).Id,
	)
	_, err := tx.Exec(sql)
	return err
}

func (table *stdTable) MarkRetry(db *sql.DB, msg Message, retryAfter time.Duration) error {
	sql := fmt.Sprintf(`
	UPDATE %s
	SET try_count = try_count + 1,  retry_at = '%s'
	WHERE id = %d
	`,
		table.name,
		time.Now().Add(retryAfter).Format(rfc3339Micro),
		msg.(*StdMessage).Id,
	)
	_, err := db.Exec(sql)
	return err
}

func (table *stdTable) MarkGivenUp(db *sql.DB, msg Message) error {
	sql := fmt.Sprintf(`
	UPDATE %s
	SET status = '%s', try_count = try_count + 1, retry_at = '%s'
	WHERE id = %d
	`,
		table.name,
		statusGivenUp, time.Now().Format(rfc3339Micro),
		msg.(*StdMessage).Id,
	)
	_, err := db.Exec(sql)
	return err
}

func (table *stdTable) ProduceMessage(tx *sql.Tx, msg Message) error {
	m := msg.(*StdMessage)
	jsonData, ok := m.Data.([]byte)
	if !ok {
		if data, err := json.Marshal(m.Data); err != nil {
			return err
		} else {
			jsonData = []byte(data)
		}
	}

	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now()
	}
	if m.RetryAt.IsZero() {
		m.RetryAt = m.CreatedAt
	}

	sql := fmt.Sprintf(`
	INSERT INTO %s
		(queue, data, status, created_at, try_count, retry_at)
	VALUES
	    (%s,    %s,   %s,     '%s',       %d,        '%s')
	`,
		table.name,
		quote(m.Queue), quote(string(jsonData)), quote(m.Status),
		m.CreatedAt.Format(rfc3339Micro), m.TryCount, m.RetryAt.Format(rfc3339Micro),
	)
	_, err := tx.Exec(sql)
	return err
}

// quote a string, removing all zero byte('\000') in it.
func quote(s string) string {
	s = strings.Replace(s, "'", "''", -1)
	s = strings.Replace(s, "\000", "", -1)
	return "'" + s + "'"
}
