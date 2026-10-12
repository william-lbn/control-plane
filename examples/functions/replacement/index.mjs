// A dependency-free replacement for the PostgreSQL example. Restoring the
// original deployment must restore its SQL code, not only environment names.
export default {
  async fetch(request) {
    return Response.json({
      code_version: 'replacement-v2',
      message: process.env.APP_MESSAGE || 'replacement',
      method: request.method,
    });
  },
};
