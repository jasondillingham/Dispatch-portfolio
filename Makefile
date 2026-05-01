#-----------------------------------------------------------------------------#
#--- Helpers
#-----------------------------------------------------------------------------#

## help: print this help message
.PHONY: help
help:
	@echo 'Usage:'
	@sed -n 's/^##//p' ${MAKEFILE_LIST} | column -t -s ':' |  sed -e 's/^/ /'

.PHONY: confirm
confirm:
	@echo -n 'Are you sure? [y/N] ' && read ans && [ $${ans:-N} = y ]

#-----------------------------------------------------------------------------#
#--- Development
#-----------------------------------------------------------------------------#

## build: build dispatch-web and dispatch-worker for the local host
.PHONY: build
build:
	@echo "Building dispatch-web + dispatch-worker (local)..."
	go build -ldflags="-s" -o=./bin/dispatch-web ./cmd/dispatch-web
	go build -ldflags="-s" -o=./bin/dispatch-worker ./cmd/dispatch-worker

## run-web: build and run dispatch-web
.PHONY: run-web
run-web: build
	./bin/dispatch-web

## run-worker: build and run dispatch-worker (one-shot pass over the mailbox)
.PHONY: run-worker
run-worker: build
	./bin/dispatch-worker

## tidy: tidy modfiles and format .go files
.PHONY: tidy
tidy:
	go mod tidy -v
	go fmt ./...

#-----------------------------------------------------------------------------#
#--- Staging (<staging-host>)
#-----------------------------------------------------------------------------#

staging_host = <staging-host>
staging_user = cms
staging_dir  = /opt/staging/dispatch

## staging-build: cross-compile both binaries for staging (linux/amd64)
.PHONY: staging-build
staging-build:
	@echo "Building for staging (linux/amd64)..."
	GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o=./bin/dispatch-web-linux ./cmd/dispatch-web
	GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o=./bin/dispatch-worker-linux ./cmd/dispatch-worker

## staging-connect: ssh to the staging server
.PHONY: staging-connect
staging-connect:
	ssh ${staging_user}@${staging_host}

## staging-prep: create target dirs on staging (one-time, idempotent)
.PHONY: staging-prep
staging-prep:
	@echo "Preparing ${staging_dir} on ${staging_host}..."
	ssh ${staging_user}@${staging_host} 'mkdir -p ${staging_dir}/bin ${staging_dir}/data'

## staging-deploy: cross-compile and rsync binaries + data to staging
.PHONY: staging-deploy
staging-deploy: staging-build staging-prep
	@echo "Deploying to staging..."
	rsync -rP ./bin/dispatch-web-linux    ${staging_user}@${staging_host}:${staging_dir}/bin/dispatch-web
	rsync -rP ./bin/dispatch-worker-linux ${staging_user}@${staging_host}:${staging_dir}/bin/dispatch-worker
	rsync -rP --delete ./data/ ${staging_user}@${staging_host}:${staging_dir}/data/
	@echo ""
	@echo "Deploy complete. Binaries live at ${staging_dir}/bin/."
	@echo "First-time setup reminders:"
	@echo "  - copy configs/msgraph_config.json + mssql_config.json into ${staging_dir}/configs/"
	@echo "  - export DISPATCH_PASSWORD in the service unit to require auth"
	@echo "  - install dispatch-web.service (see make staging-install-service) to run as systemd"

## staging-install-service: install systemd unit and enable dispatch-web
.PHONY: staging-install-service
staging-install-service:
	@echo "Installing dispatch-web.service on staging..."
	rsync -P ./deploy/dispatch-web.service ${staging_user}@${staging_host}:/tmp/dispatch-web.service
	ssh ${staging_user}@${staging_host} 'sudo mv /tmp/dispatch-web.service /etc/systemd/system/dispatch-web.service && sudo systemctl daemon-reload && sudo systemctl enable dispatch-web'
	@echo "Service installed + enabled. Start with 'make staging-start'."

## staging-logs: follow dispatch-web journald logs on staging
.PHONY: staging-logs
staging-logs:
	ssh -t ${staging_user}@${staging_host} 'sudo journalctl --unit=dispatch-web --since="24 hours ago" --follow'

## staging-status: show dispatch-web service status
.PHONY: staging-status
staging-status:
	@ssh ${staging_user}@${staging_host} 'sudo systemctl status dispatch-web --no-pager'

## staging-start: start dispatch-web
.PHONY: staging-start
staging-start:
	ssh ${staging_user}@${staging_host} 'sudo systemctl start dispatch-web'
	@echo "dispatch-web started"

## staging-stop: stop dispatch-web
.PHONY: staging-stop
staging-stop:
	ssh ${staging_user}@${staging_host} 'sudo systemctl stop dispatch-web'
	@echo "dispatch-web stopped"

## staging-restart: restart dispatch-web (use after staging-deploy)
.PHONY: staging-restart
staging-restart:
	ssh ${staging_user}@${staging_host} 'sudo systemctl restart dispatch-web'
	@echo "dispatch-web restarted"

## staging-run-worker: run dispatch-worker once on staging (foreground, useful for testing)
.PHONY: staging-run-worker
staging-run-worker:
	ssh -t ${staging_user}@${staging_host} 'cd ${staging_dir} && ./bin/dispatch-worker'
