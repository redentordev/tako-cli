require 'sinatra'

set :bind, '0.0.0.0'
set :port, 3000
set :environment, :production

get '/' do
  <<-HTML
    <!DOCTYPE html>
    <html>
    <head>
      <title>Ruby/Sinatra on Tako CLI</title>
      <style>
        body {
          font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif;
          max-width: 800px;
          margin: 50px auto;
          padding: 20px;
          line-height: 1.6;
        }
        h1 { color: #DC143C; }
        .info { background: #f4f4f4; padding: 15px; border-radius: 5px; }
      </style>
    </head>
    <body>
      <h1>🎉 Ruby/Sinatra is Running!</h1>
      <p>This is a Ruby Sinatra application deployed with Tako CLI.</p>
      
      <div class="info">
        <h3>About this deployment:</h3>
        <ul>
          <li>Framework: Sinatra #{Sinatra::VERSION}</li>
          <li>Ruby: #{RUBY_VERSION}</li>
          <li>Environment: #{settings.environment}</li>
          <li>Server: Puma</li>
          <li>Deployed with: Tako CLI</li>
        </ul>
      </div>
      
      <p><a href="/api">Try the API endpoint →</a></p>
    </body>
    </html>
  HTML
end

get '/api' do
  content_type :json
  {
    status: 'success',
    message: 'Hello from Sinatra API!',
    ruby_version: RUBY_VERSION,
    sinatra_version: Sinatra::VERSION,
    timestamp: Time.now.iso8601
  }.to_json
end

get '/health' do
  content_type :json
  { status: 'healthy' }.to_json
end
