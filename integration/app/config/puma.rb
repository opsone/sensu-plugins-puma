workers 2
threads 1, 2
bind "tcp://127.0.0.1:#{ENV.fetch('PORT')}"

# Same setup as documented in the README.
if ENV['PUMA_CONTROL_PORT']
  state_path 'tmp/pids/puma.state'
  activate_control_app "tcp://127.0.0.1:#{ENV['PUMA_CONTROL_PORT']}", data_only: true
end
