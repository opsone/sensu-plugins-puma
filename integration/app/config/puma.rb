workers 2
threads 1, 2
bind "tcp://127.0.0.1:#{ENV.fetch('PORT')}"

# Same setups as documented in the README. data_only exists since Puma 7.2 (ignored before).
if ENV['PUMA_CONTROL_PORT']
  url = "tcp://127.0.0.1:#{ENV['PUMA_CONTROL_PORT']}"
  data_only = Gem::Version.new(Puma::Const::PUMA_VERSION) >= Gem::Version.new('7.2')
  if ENV['PUMA_CONTROL_TOKEN']
    # Control app only, fixed token, given to the check with --control and --control-token.
    activate_control_app url, auth_token: ENV['PUMA_CONTROL_TOKEN']
  elsif ENV['PUMA_NO_STATE']
    # Control app only, no token (Puma 7.2+), given to the check with --control.
    activate_control_app url, no_token: true, data_only: true
  else
    state_path 'tmp/pids/puma.state'
    data_only ? activate_control_app(url, data_only: true) : activate_control_app(url)
  end
end
