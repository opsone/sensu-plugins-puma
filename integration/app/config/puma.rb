workers 2
threads 1, 2
bind "tcp://127.0.0.1:#{ENV.fetch('PORT')}"

# Same setups as documented in the README.
if ENV['PUMA_CONTROL_PORT'] && ENV['PUMA_NO_STATE']
  # Control app only, no state file: given to the check with --control.
  activate_control_app "tcp://127.0.0.1:#{ENV['PUMA_CONTROL_PORT']}", no_token: true, data_only: true
elsif ENV['PUMA_CONTROL_PORT']
  state_path 'tmp/pids/puma.state'
  activate_control_app "tcp://127.0.0.1:#{ENV['PUMA_CONTROL_PORT']}", data_only: true
end
