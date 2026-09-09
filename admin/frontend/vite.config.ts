// Copyright (c) 2026 Ivan Vasilev
// This source code is licensed under the MIT license found in the
// LICENSE file in the root directory of this source tree.
import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

// В деве Vite (:5173) проксирует запросы на бэкенд (:8080) — фронт всегда
// ходит по относительным путям, порт нигде не зашит.
export default defineConfig({
    plugins: [react()],
    server: {
        proxy: {
            '/api':    'http://localhost:8080',
            '/login':  'http://localhost:8080',
            '/logout': 'http://localhost:8080',
        },
    },
});
