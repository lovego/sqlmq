package sqlmq

import (
	"context"
	"database/sql"
	"time"

	"github.com/lovego/logger"
)

func (mq *SqlMQ) Consume() {
	if err := mq.validate(); err != nil {
		panic(time.Now().Format(time.RFC3339Nano) + " " + err.Error())
	}
	if mq.CleanInterval > 0 {
		go mq.clean()
	}

	idleWait, errorWait := mq.getWaitTime()
	mq.consumeNotify = make(chan struct{}, 1)

	var wait time.Duration
	for {
		wait = mq.consume(idleWait, errorWait)
		if wait > 0 {
			select {
			case <-time.NewTimer(wait).C:
			case <-mq.consumeNotify:
			}
		}
	}
}

func (mq *SqlMQ) consume(idleWait, errorWait time.Duration) time.Duration {
	if mq.noQueues() {
		return idleWait
	}
	for {
		if wait, err := mq.consumeOne(idleWait); err != nil {
			mq.Logger.Error(err)
			return errorWait
		} else if wait > 0 {
			if wait > idleWait {
				wait = idleWait
			}
			return wait
		}
	}
}

func (mq *SqlMQ) consumeOne(idleWait time.Duration) (wait time.Duration, err error) {
	tx, cancel, err := mq.beginTx()
	if err != nil {
		return
	}

	msg, err := mq.Table.EarliestMessage(tx)
	if msg != nil {
		wait = time.Until(msg.ConsumeAt())
	} else {
		wait = idleWait
	}
	if wait > 0 || err != nil {
		if err2 := tx.Rollback(); err2 != nil {
			mq.Logger.Error(err2)
		}
		cancel()
		return
	}

	var retryAfter time.Duration
	var handleErr error
	go mq.Logger.Record(func(ctx context.Context) error {
		retryAfter, handleErr = mq.handle(ctx, cancel, tx, msg)
		return handleErr
	}, nil, func(f *logger.Fields) {
		f.With("message", msg)
		if handleErr != nil {
			f.With("retryAfter", retryAfter.String())
		}
	})

	return
}

func (mq *SqlMQ) handle(ctx context.Context, cancel func(), tx *sql.Tx, msg Message) (
	retryAfter time.Duration, err error,
) {
	var canCommit bool
	defer func() {
		if err == nil {
			err = tx.Commit()
		} else if canCommit {
			if err2 := tx.Commit(); err2 != nil {
				mq.Logger.Error(err2)
			}
		} else {
			if err2 := tx.Rollback(); err2 != nil {
				mq.Logger.Error(err2)
			}
		}
		cancel()
	}()

	handler, err := mq.handlerOf(msg)
	if err == nil {
		if retryAfter, canCommit, err = handler(ctx, tx, msg); err == nil {
			err = mq.Table.MarkSuccess(tx, msg)
		} else if canCommit {
			mq.markFail(tx, msg, retryAfter)
		} else {
			// Do this before transaction released the "FOR UPDATE" lock.
			go mq.markFail(mq.DB, msg, retryAfter)
			// Wait the goroutine above to be ready to preempt the lock before rollback release the lock.
			// Reduce the rate that `EarliestMessage` got the lock and consume this message again.
			time.Sleep(100 * time.Millisecond)
		}
	} else {
		canCommit = true
		mq.markFail(tx, msg, time.Minute)
	}
	return
}
func (mq *SqlMQ) markFail(db DBOrTx, msg Message, retryAfter time.Duration) {
	if retryAfter >= 0 {
		if err := mq.Table.MarkRetry(db, msg, retryAfter); err != nil {
			mq.Logger.Error(err)
		} else {
			mq.TriggerConsume()
		}
	} else {
		if err := mq.Table.MarkGivenUp(db, msg); err != nil {
			mq.Logger.Error(err)
		}
	}
}

func (mq *SqlMQ) beginTx() (*sql.Tx, func(), error) {
	txTimeout := mq.TxTimeout
	if txTimeout <= 0 {
		txTimeout = time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), txTimeout)
	tx, err := mq.DB.BeginTx(ctx, nil)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	return tx, cancel, err
}

func (mq *SqlMQ) getWaitTime() (idleWait, errorWait time.Duration) {
	idleWait, errorWait = mq.IdleWait, mq.ErrorWait
	if idleWait <= 0 {
		idleWait = time.Minute
	}
	if errorWait <= 0 {
		errorWait = time.Minute
	}
	return
}

func (mq *SqlMQ) clean() {
	for {
		var cleaned int64
		var err error
		mq.Logger.Record(func(ctx context.Context) error {
			cleaned, err = mq.Table.CleanMessages(mq.DB)
			return err
		}, nil, func(f *logger.Fields) {
			f.With("cleaned", cleaned)
		})
		time.Sleep(mq.CleanInterval)
	}
}
