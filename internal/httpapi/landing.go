package httpapi

// rootHTML is the gateway's landing page, served at /.
// It lives here rather than in internal/web because it is inlined in the
// binary as a constant, not served from the embedded asset filesystem.
const rootHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<style>
.fire-ring {
    display: flex;
    align-items: center;
    justify-content: center;
    margin: auto;
    width: 283px;
    height: 283px;
    border: 8px solid #8e1248;
    border-radius: 50%;
    padding: 10px;
    font-size: 128px;
}
.thanks-fire-ring {
    padding: 24px;
    text-align: center;
    font-size: 32px;
}
.fire-ring-text { color: #8e1248; }
.card {
    margin: 0;
    position: absolute;
    top: 50%;
    left: 50%;
    -ms-transform: translate(-50%, -50%);
    transform: translate(-50%, -50%);
    font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", "Roboto", "Helvetica Neue", Arial, sans-serif, "Apple Color Emoji", "Segoe UI Emoji", "Segoe UI Symbol";
}
</style>
<title>FireRing</title>
</head>
<body>
<div class="card">
    <div class="fire-ring"><p class="fire-ring-text">🔥</p></div>
    <div class="thanks-fire-ring">
        <p>Grateful to the <span class="fire-ring-text">FireRing</span><br>Praise the <span class="fire-ring-text">FireRing</span></p>
    </div>
</div>
</body>
</html>`
