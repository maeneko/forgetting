// Copyright (c) 2026 Ivan Vasilev
// This source code is licensed under the MIT license found in the
// LICENSE file in the root directory of this source tree.
import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

const api = `http://localhost:${process.env.MOCK_PORT || 8080}`;

export default defineConfig({
    // Ресурсы — от адреса страницы, а не от корня домена: собранная панель работает и в корне, и в
    // подпапке за reverse-proxy (example.com/forgetting/) без пересборки. Запросы к API — так же
    // (main.tsx).
    base: './',
    plugins: [react()],
    server: {
        proxy: {
            '/api':    api,
            '/ui':     api,
            '/health': api,
            '/login':  api,
            '/logout': api,
        },
    },
});
