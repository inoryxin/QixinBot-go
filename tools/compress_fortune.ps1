// 在远程把 fortune 的 955 张底图压成 480x480 JPEG（质量 82），减少 90% 体积
Add-Type -AssemblyName System.Drawing

$src = 'C:\Users\server\onebot-bot\data\plugins\fortune\assets\img'
$dst = 'C:\Users\server\fortune-compressed\img'
New-Item -ItemType Directory -Force -Path $dst | Out-Null

$codec = [System.Drawing.Imaging.ImageCodecInfo]::GetImageEncoders() | Where-Object { $_.MimeType -eq 'image/jpeg' }
$params = New-Object System.Drawing.Imaging.EncoderParameters(1)
$params.Param[0] = New-Object System.Drawing.Imaging.EncoderParameter([System.Drawing.Imaging.Encoder]::Quality, [long]82)

$total = 0
$done = 0
Get-ChildItem $src -Directory | ForEach-Object {
    $theme = $_.Name
    $outDir = Join-Path $dst $theme
    New-Item -ItemType Directory -Force -Path $outDir | Out-Null
    Get-ChildItem $_.FullName -File | Where-Object { $_.Extension -match '^\.(png|jpe?g|webp)$' } | ForEach-Object {
        $total++
        $out = Join-Path $outDir ($_.BaseName + '.jpg')
        try {
            $img = [System.Drawing.Image]::FromFile($_.FullName)
            $bmp = New-Object System.Drawing.Bitmap 480, 480
            $g = [System.Drawing.Graphics]::FromImage($bmp)
            $g.InterpolationMode = [System.Drawing.Drawing2D.InterpolationMode]::HighQualityBicubic
            $g.DrawImage($img, 0, 0, 480, 480)
            $bmp.Save($out, $codec, $params)
            $g.Dispose(); $bmp.Dispose(); $img.Dispose()
            $done++
        } catch {
            Write-Output "FAIL $($_.FullName): $($_.Exception.Message)"
        }
    }
}

$sum = (Get-ChildItem $dst -Recurse -File | Measure-Object Length -Sum)
Write-Output "压缩完成: $done / $total 张, 输出 $([math]::Round($sum.Sum/1MB,1)) MB"
