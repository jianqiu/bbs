package taskworkpool

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"regexp"

	"code.cloudfoundry.org/bbs/db"
	"code.cloudfoundry.org/bbs/models"
	"code.cloudfoundry.org/cfhttp"
	"code.cloudfoundry.org/lager"
	"code.cloudfoundry.org/workpool"
)

const MAX_CB_RETRIES = 3

//go:generate counterfeiter . TaskCompletionClient

type CompletedTaskHandler func(logger lager.Logger, httpClient *http.Client, taskDB db.TaskDB, task *models.Task)

type TaskCompletionClient interface {
	Submit(taskDB db.TaskDB, task *models.Task)
}

type TaskCompletionWorkPool struct {
	logger           lager.Logger
	maxWorkers       int
	callbackHandler  CompletedTaskHandler
	callbackWorkPool *workpool.WorkPool
	httpClient       *http.Client
}

func New(logger lager.Logger, maxWorkers int, cbHandler CompletedTaskHandler) *TaskCompletionWorkPool {
	if cbHandler == nil {
		panic("callbackHandler cannot be nil")
	}
	return &TaskCompletionWorkPool{
		logger:          logger.Session("task-completion-workpool"),
		maxWorkers:      maxWorkers,
		callbackHandler: cbHandler,
		httpClient:      cfhttp.NewClient(),
	}
}

func (twp *TaskCompletionWorkPool) Run(signals <-chan os.Signal, ready chan<- struct{}) error {
	cbWorkPool, err := workpool.NewWorkPool(twp.maxWorkers)
	logger := twp.logger
	logger.Info("starting")

	if err != nil {
		logger.Error("creation-failed", err)
		return err
	}
	twp.callbackWorkPool = cbWorkPool
	close(ready)
	logger.Info("started")
	defer logger.Info("finished")

	<-signals
	go twp.callbackWorkPool.Stop()

	return nil
}

func (twp *TaskCompletionWorkPool) Submit(taskDB db.TaskDB, task *models.Task) {
	if twp.callbackWorkPool == nil {
		panic("called submit before workpool was started")
	}
	logger := twp.logger
	twp.callbackWorkPool.Submit(func() {
		twp.callbackHandler(logger, twp.httpClient, taskDB, task)
	})
}

func HandleCompletedTask(logger lager.Logger, httpClient *http.Client, taskDB db.TaskDB, task *models.Task) {
	logger = logger.Session("handle-completed-task", lager.Data{"task_guid": task.TaskGuid})

	if task.CompletionCallbackUrl != "" {
		modelErr := taskDB.ResolvingTask(logger, task.TaskGuid)
		if modelErr != nil {
			logger.Error("marking-task-as-resolving-failed", modelErr)
			return
		}

		logger = logger.WithData(lager.Data{"callback_url": task.CompletionCallbackUrl})

		json, err := json.Marshal(&models.TaskCallbackResponse{
			TaskGuid:      task.TaskGuid,
			Failed:        task.Failed,
			FailureReason: task.FailureReason,
			Result:        task.Result,
			Annotation:    task.Annotation,
			CreatedAt:     task.CreatedAt,
		})
		if err != nil {
			logger.Error("marshalling-task-failed", err)
			return
		}

		var statusCode int

		for i := 0; i < MAX_CB_RETRIES; i++ {
			request, err := http.NewRequest("POST", task.CompletionCallbackUrl, bytes.NewReader(json))
			if err != nil {
				logger.Error("building-request-failed", err)
				return
			}

			request.Header.Set("Content-Type", "application/json")
			response, err := httpClient.Do(request)
			if err != nil {
				matched, _ := regexp.MatchString("Client.Timeout|use of closed network connection", err.Error())
				if matched {
					continue
				}
				logger.Error("doing-request-failed", err)
				return
			}
			defer response.Body.Close()

			statusCode = response.StatusCode
			if shouldResolve(statusCode) {
				modelErr := taskDB.DeleteTask(logger, task.TaskGuid)
				if modelErr != nil {
					logger.Error("delete-task-failed", modelErr)
				}
				return
			}
		}

		logger.Info("callback-failed", lager.Data{"status_code": statusCode})
	}
	return
}

func shouldResolve(status int) bool {
	switch status {
	case http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return false
	default:
		return true
	}
}
