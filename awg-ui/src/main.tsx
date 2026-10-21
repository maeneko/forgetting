// Copyright (c) 2026 Ivan Vasilev
// This source code is licensed under the MIT license found in the
// LICENSE file in the root directory of this source tree.
import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import axios from 'axios';
import App from './App';

// Пути к API в коде пишутся от корня («/ui/nodes»), а уходят от адреса страницы («ui/nodes»):
// так панель за reverse-proxy в подпапке (example.com/forgetting/) ходит в свой префикс, а в корне
// всё как раньше. Одно место на все запросы, включая будущие.
axios.interceptors.request.use(c => {
    if (c.url?.startsWith('/') && !c.url.startsWith('//')) c.url = c.url.slice(1);
    return c;
});

createRoot(document.getElementById('root')!).render(
    <StrictMode>
        <App />
    </StrictMode>
);