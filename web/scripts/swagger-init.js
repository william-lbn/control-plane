window.onload = () => {
  const query = new URLSearchParams(window.location.search),
    branch = query.get('branch_id');
  const suffix =
    branch && /^br_[a-f0-9]{16}$/.test(branch) ? '?branch_id=' + encodeURIComponent(branch) : '';
  SwaggerUIBundle({
    urls: [
      { name: 'Control API', url: '/api/openapi.json' },
      { name: 'Application Auth', url: '/api/application-auth-openapi.json' + suffix },
    ],
    'urls.primaryName': query.get('schema') === 'auth' ? 'Application Auth' : 'Control API',
    dom_id: '#swagger-ui',
    presets: [SwaggerUIBundle.presets.apis, SwaggerUIStandalonePreset],
    layout: 'StandaloneLayout',
    persistAuthorization: false,
    withCredentials: true,
  });
};
