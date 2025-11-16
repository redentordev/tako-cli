<?php

use Illuminate\Support\Facades\Route;

Route::get('/', function () {
    return view('welcome');
});

Route::get('/api/hello', function () {
    return response()->json([
        'message' => 'Hello from Laravel!',
        'framework' => 'Laravel',
        'version' => app()->version(),
        'deployed_with' => 'Tako CLI',
        'timestamp' => now()->toIso8601String(),
        'features' => [
            'Eloquent ORM',
            'Blade templating',
            'Routing',
            'Middleware',
            'Authentication',
            'Queue system'
        ]
    ]);
});

Route::get('/api/status', function () {
    return response()->json([
        'status' => 'healthy',
        'laravel_version' => app()->version(),
        'php_version' => PHP_VERSION,
        'environment' => app()->environment(),
        'debug_mode' => config('app.debug'),
        'timestamp' => now()->toIso8601String()
    ]);
});
