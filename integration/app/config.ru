# /sleep keeps a thread busy, to saturate the pool.
run lambda { |env|
  sleep 8 if env["PATH_INFO"] == "/sleep"
  [200, { "content-type" => "text/plain" }, ["ok\n"]]
}
