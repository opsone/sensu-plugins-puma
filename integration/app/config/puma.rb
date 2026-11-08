workers 2
threads 1, 2
bind "tcp://127.0.0.1:#{ENV.fetch('PORT')}"
state_path "tmp/pids/puma.state"
activate_control_app "unix://tmp/pumactl.sock", auth_token: "s3cr3t"
