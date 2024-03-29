.PHONY: up down logs bench restart kataribe

up:
	docker-compose up -d --build

down:
	docker-compose down

logs:
	docker-compose logs -f

bench: workload=2
bench: init=./init.sh
bench:
	sudo /opt/isucon3-mod/bench/bench benchmark --workload ${workload} --init ${init}

SLOW_LOG=/var/lib/mysql/mysql-slow.log
restart:
	$(MAKE) -C app build && sudo systemctl restart isucon.golang.service
	sudo bash -c "echo '' > /var/log/nginx/access.log && echo '' > /var/log/nginx/error.log" && sudo systemctl restart nginx.service
	sudo bash -c "echo '' > $(SLOW_LOG)" && sudo systemctl restart mysqld.service

kataribe:
	sudo cat /var/log/nginx/access.log | kataribe

slow-query:
	sudo mysqldumpslow -t 10 /var/lib/mysql/mysql-slow.log

BRUNCH := master

all-deploy:
	ssh  isucon-app-1 "cd /opt/isucon3-mod; make deploy BRUNCH=${BRUNCH} " & \
	ssh  isucon-app-2 "cd /opt/isucon3-mod; make deploy BRUNCH=${BRUNCH}"

deploy:
	git checkout ${BRUNCH}
	git pull
	make restart
